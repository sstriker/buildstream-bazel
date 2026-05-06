# Generated workspace structure (interop contract)

Documents the layout `cmd/write-a` produces and the contracts a
sibling tool would have to honor to interop. Two directories
matter: **project A** (the meta workspace whose genrules invoke
per-kind translators) and **project B** (the consumer workspace
that builds the final artifacts). For the architecture in
context see [docs/overview.md](overview.md).

## Project A — the meta workspace

```
project-A/
├── MODULE.bazel            # bazel_dep(name = "bazel_skylib", version = "1.7.1")
├── BUILD.bazel             # workspace-root marker (empty defaults)
├── rules/
│   ├── zero_files.bzl      # path → zero-length stub generator
│   ├── sources.bzl         # @src_<key>// repo rule (FUSE-sources mode)
│   └── BUILD.bazel         # bzl_library exports for the .bzl files
├── tools/
│   ├── convert-element             # the cmake converter binary
│   ├── convert-element-autotools   # the autotools converter binary
│   ├── build-tracer                # process tracer binary
│   ├── sources.json                # source-key → URL/digest catalogue
│   └── BUILD.bazel                 # exports_files([...]) for the above
└── elements/
    └── <element-name>/
        ├── BUILD.bazel             # per-element render
        ├── sources/                # staged kind:local sources (default mode)
        │   └── ...                 # source tree mirrored verbatim
        └── imports.json            # cross-element label map (kind:cmake/autotools when deps present)
```

`bazel build //elements/<name>:<name>_converted` (or
`<name>_install` for non-cmake kinds) materializes the
per-element outputs. The driver script then stages those
outputs into project B.

### Per-element BUILD shapes by kind

#### kind:cmake (single-element, no cross-element deps)

```python
package(default_visibility = ["//visibility:public"])

filegroup(
    name = "hello_real",
    srcs = glob(["sources/**"]),
)

genrule(
    name = "hello_converted",
    srcs = [":hello_real"],
    outs = [
        "BUILD.bazel.out",            # cc_library / cc_binary IR
        "read_paths.json",            # narrowing feedback for next render
        "cmake-config-bundle.tar",    # cross-element synth prefix
    ],
    cmd = """
        SHADOW="$$(mktemp -d)"
        for src in $(SRCS); do
            rel="$${src##*sources/}"
            mkdir -p "$$SHADOW/$$(dirname "$$rel")"
            cp -L "$$src" "$$SHADOW/$$rel"
        done
        BUNDLE_DIR="$$(mktemp -d)"
        $(location //tools:convert-element) \
            --source-root="$$SHADOW" \
            --out-build="$(location BUILD.bazel.out)" \
            --out-bundle-dir="$$BUNDLE_DIR" \
            --out-read-paths="$(location read_paths.json)"
        tar -cf "$(location cmake-config-bundle.tar)" -C "$$BUNDLE_DIR" .
    """,
    tools = ["//tools:convert-element"],
)

filegroup(name = "build_bazel",          srcs = ["BUILD.bazel.out"])
filegroup(name = "cmake_config_bundle",  srcs = ["cmake-config-bundle.tar"])
```

#### kind:cmake (with cross-element deps)

`<elem>_converted`'s `srcs` includes
`//elements/<dep>:cmake_config_bundle` for each kind:cmake dep
plus `imports.json` (rendered at write-a time, mapping the dep's
IMPORTED-target name to its Bazel label). The `cmd` extracts each
dep's bundle into `$PREFIX/lib/cmake/<dep>/`, passes
`--prefix-dir=$PREFIX` so `find_package(<DepPkg> CONFIG)` resolves,
and passes `--imports-manifest=$(location imports.json)` so
IMPORTED-target names map back to Bazel labels.

#### kind:autotools (trace-driven native render, round-2 default)

Round-2 is the default. The converter lives in **project A** (pass 2);
the install build lives in **project B** (pass 3) and publishes the
trace to the REAPI ActionCache so the converter can read it on the
next pass-2 run.

**Project A — per-element converter genrule (`<elem>_build`):**

```python
# kind:autotools round-2 — pass-2 converter genrule.
# Reads @trace_greet//:trace (load-time AC lookup via rules/traces.bzl).
# AC hit ⇒ emits cc_library / cc_binary into BUILD.bazel.out.
# AC miss ⇒ emits a placeholder BUILD.bazel.out.
genrule(
    name = "greet_build",
    srcs = [
        "@src_<key>//:tree",        # source tree
        "@trace_greet//:trace",     # AC-backed trace (trace.log + make-db.txt)
        "srckey.txt",               # seed for the synthetic AC key
    ],
    outs = ["BUILD.bazel.out"],
    tools = ["//tools:convert-element-autotools"],
    cmd = """
        TRACE_DIR="$$(mktemp -d)"
        for src in $(SRCS); do
            case "$$src" in
                */trace_dir/trace.log)    cp -L "$$src" "$$TRACE_DIR/trace.log"    ;;
                */trace_dir/make-db.txt)  cp -L "$$src" "$$TRACE_DIR/make-db.txt"  ;;
            esac
        done
        $(location //tools:convert-element-autotools) \
            --trace-dir="$$TRACE_DIR" \
            --out-build="$(location BUILD.bazel.out)"
    """,
)
```

**Project B — coarse install genrule (`<elem>_install`):**

```python
genrule(
    name = "greet_install",
    srcs = [":greet_sources", "srckey.txt"],
    outs = [
        "install_tree.tar",       # the build's DESTDIR install tree
        "trace.log",              # canonicalized execve trace
        "make-db.txt",            # `make -np` post-build dump
        "generated-headers.txt",  # generated header list (for future use)
    ],
    cmd = """
        # ...source staging + dep-prefix extraction...
        export INSTALL_ROOT="$$(mktemp -d)"
        export AUTOTOOLS_TRACE="$$(mktemp)"
        $(location //tools:build-tracer) \
            --out="$$AUTOTOOLS_TRACE" -- sh -c '
            ./configure --prefix=/usr ...
            make
            make -j1 DESTDIR="$$INSTALL_ROOT" install
        '
        # post-process make-db (strip non-deterministic lines)
        ( make -np 2>/dev/null || true ) | sed -E '...' \
            > "$$EXEC_ROOT/$(location make-db.txt)"
        cp -L "$$AUTOTOOLS_TRACE" "$$EXEC_ROOT/$(location trace.log)"
        tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
            -cf "$(location install_tree.tar)" -C "$$INSTALL_ROOT" .
        # Publish trace → AC under SyntheticActionDigest(srckey)
        if [ -n "$${CAS_GRPC_ADDR:-}" ]; then
            $(location //tools:trace-publish) \
                --cas="$${CAS_GRPC_ADDR}" \
                --srckey="$$(cat $(location srckey.txt) | tr -d '[:space:]')" \
                --trace="$(location trace.log)" \
                --make-db="$(location make-db.txt)" >/dev/null
        fi
    """,
    tools = [
        "//tools:build-tracer",
        "//tools:trace-publish",
    ],
)
```

The converter genrule (project A) and the install genrule (project B)
are independent Bazel actions. `@trace_<elem>//:trace` is a Bazel
external repo backed by `rules/traces.bzl`: at load time it shells
out to `cmd/trace-lookup`, which queries the REAPI ActionCache under
the element's synthetic key (see `docs/design/autotools-round2-rendezvous.md`).
AC hit ⇒ the fileset resolves to `trace.log` + `make-db.txt` and the
converter emits fine-grained cc rules. AC miss ⇒ the fileset is empty
and the converter writes a placeholder; project B's install genrule
runs, publishes the trace, and the next render of project A hits.

Pass `--autotools-round1` to `write-a` to opt into the legacy
single-genrule shape (converter + install inline in one project-B
action). See `docs/trace-driven-autotools.md` for that shape.

When the element has cross-element deps, an `imports.json` is rendered
and listed in the project-A genrule's `srcs`, mirroring the
kind:cmake handler's contract.

#### Pipeline kinds with trace-driven path (kind:make, kind:manual, kind:script, kind:makemaker, kind:modulebuild)

Same round-2 shape as autotools: project A holds a converter genrule
(`<elem>_build`); project B holds the install genrule (`<elem>_install`)
whose outputs include `install_tree.tar`, `trace.log`, `make-db.txt`.
Pipeline kinds without a trace-driven opt-in produce only
`install_tree.tar` from `<elem>_install`.

#### Filegroup-composition kinds (kind:stack / kind:compose / kind:filter / kind:import)

No genrule. `BUILD.bazel` is pure starlark filegroup composition
of dep elements' install trees:

```python
package(default_visibility = ["//visibility:public"])

filegroup(
    name = "runtime",
    srcs = [
        "//elements/lib-a:lib-a_install",
        "//elements/lib-b:lib-b_install",
    ],
)
```

## Project B — the consumer workspace

```
project-B/
├── MODULE.bazel            # bazel_dep(name = "rules_cc", version = ...)
├── BUILD.bazel             # workspace-root marker
└── elements/
    └── <element-name>/
        ├── BUILD.bazel     # initially BUILD_NOT_YET_STAGED placeholder;
        │                   # overwritten with project A's BUILD.bazel.out
        │                   # by the driver script after pass 1 completes.
        └── ...source files (mirrored from kind:local) or
            ...staged trees (kind:import)
```

Once the driver stages each element's `BUILD.bazel.out` into
project B, `bazel build //...` over project B compiles the
fully native graph: every `kind:cmake` and trace-driven
`kind:autotools` element becomes one or more `cc_library` /
`cc_binary` rules; pipeline-kind installs surface as filegroups
that consumers reference via the install_tree.tar shape.

## Cross-element label conventions

A sibling tool (or any consumer importing the generated
workspace) sees a stable label namespace:

| Label pattern | Meaning |
|---|---|
| `//elements/<name>:<name>` | Primary cc rule (cc_library / cc_binary) for kind:cmake / native kind:autotools. |
| `//elements/<name>:<name>_install` | Install genrule (project B). Outputs include `install_tree.tar`; trace-driven kinds also emit `trace.log` / `make-db.txt`. Does NOT emit `BUILD.bazel.out` under round-2 (that is `<name>_build` in project A). |
| `//elements/<name>:<name>_build` | Round-2 converter genrule (project A, trace-driven kinds). Reads trace from AC; outputs `BUILD.bazel.out`. |
| `//elements/<name>:<name>_converted` | The cmake convert genrule (kind:cmake only, project A). Outputs `BUILD.bazel.out` + `read_paths.json` + `cmake-config-bundle.tar`. |
| `//elements/<name>:cmake_config_bundle` | Synthesized cmake-config bundle tar, for cross-element find_package consumption. |
| `//elements/<name>:build_bazel` | The rendered `BUILD.bazel.out` filegroup (one entry: the file itself). |

For **kind:cmake → kind:cmake** consumers: depend on
`//elements/<dep>:cmake_config_bundle`. The producer ships
`<DepPkg>Config.cmake` + IMPORTED-location stubs in the bundle;
extracting it under `$PREFIX/lib/cmake/<DepPkg>/` lets
`find_package` resolve.

For **kind:autotools native → cross-element** consumers: the
producer's `//elements/<dep>:<dep>` Bazel label is a real
cc_library / cc_binary; depend on it directly. No bundle
extraction needed.

For **pipeline-kind (install-tree) deps**: depend on
`//elements/<dep>:<dep>_install`'s install_tree.tar via
filegroup composition; consumers then unpack the tar.

## Interop contract — what a sibling tool should honor

If you're writing a different .bst → Bazel converter (or a
non-.bst frontend that produces the same workspace shape), here's
the surface area:

1. **Workspace pair.** Render two Bazel workspaces (project A +
   project B) with the directory shapes above.
2. **Per-element packages** under
   `elements/<safe-element-name>/`. Element name normalization:
   strip the `.bst` suffix; otherwise pass through (BuildStream
   element names are already filesystem-safe).
3. **Per-element label conventions** (see table above) so any
   consumer can resolve cross-element deps without
   tool-specific knowledge.
4. **Per-kind genrule shapes.** kind:cmake / kind:autotools-native
   produce `BUILD.bazel.out` + sidechannels. Pipeline kinds
   produce `install_tree.tar`. Filegroup-composition kinds
   produce no genrule, just starlark filegroups.
5. **Project-A-output → Project-B-input staging.** After
   `bazel build //...` over project A, every kind that emitted
   a `BUILD.bazel.out` has its content staged into project B's
   `elements/<name>/BUILD.bazel`. The driver replaces the
   placeholder.
6. **Tools staged in project A.** The translators
   (`convert-element`, `convert-element-autotools`,
   `build-tracer`) live under `project-A/tools/` and are
   referenced via `//tools:<name>` labels from per-element
   genrules' `tools = [...]`. The translator binary contract
   (CLI flags, output shape) is documented per-tool.
7. **Convergence via Bazel's action cache.** For kind:cmake there is
   no separate srckey registry — rely on Bazel's existing remote-cache
   plumbing. For trace-driven kinds (`cmd/trace-publish` /
   `cmd/trace-lookup`) the REAPI ActionCache doubles as a
   srckey → trace registry; no separate registry service is required.

The translator binaries themselves (`convert-element` etc.) are
the implementation detail of *this* converter — a sibling tool
could replace them with its own per-kind translators as long as
the generated workspace's label / output / staging contracts
stay the same.

## Translator CLI contracts

For the per-element genrule's tools, here's what each binary
expects:

### convert-element (kind:cmake)

```
convert-element \
    --source-root=<dir>                  # cmake source tree
    --out-build=<path>                   # write BUILD.bazel.out here
    --out-bundle-dir=<dir>               # write cmake-config bundle here
    --out-read-paths=<path>              # write narrowing feedback here
    [--prefix-dir=<dir>]                 # cross-element synth prefix
    [--imports-manifest=<path>]          # cross-element label map
    [--trace-path=<path>]                # cmake --trace-expand output
```

Outputs:
- `BUILD.bazel.out`: native cc_library / cc_binary rules.
- `<bundle-dir>/lib/cmake/<Pkg>/...`: synth cmake-config files.
- `read_paths.json`: include-paths cmake actually read (narrowing).

### convert-element-autotools (kind:autotools native)

```
convert-element-autotools \
    --trace=<path>                       # strace text-format trace
    --out-build=<path>                   # write BUILD.bazel.out here
    [--imports-manifest=<path>]          # cross-element label map
```

Outputs:
- `BUILD.bazel.out`: native cc_library / cc_binary rules.

### build-tracer (universal)

```
build-tracer [--strace] --out=<path> -- <cmd> [args...]
```

Wraps `<cmd>` under ptrace (default) or strace (with `--strace`),
captures every successful execve into the trace artifact at
`<path>`. Trace format is strace's text format (compatible across
both backends).
