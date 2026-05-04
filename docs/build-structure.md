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

#### kind:autotools (trace-driven native render)

```python
genrule(
    name = "greet_install",
    srcs = [":greet_sources"],
    outs = [
        "install_tree.tar",       # the build's DESTDIR install tree
        "make-db.txt",            # `make -np` post-build dump
        "BUILD.bazel.out",        # native cc rules recovered from trace
        "install-mapping.json",   # install dest -> producing rule
    ],
    cmd = """
        # ...source staging (same shadow-merge shape as kind:cmake)...
        export INSTALL_ROOT="$$(mktemp -d)"
        export AUTOTOOLS_TRACE="$$(mktemp)"
        "$$EXEC_ROOT/$(location //tools:build-tracer)" \
            --out="$$AUTOTOOLS_TRACE" -- sh -c '
            ./configure --prefix=/usr ...
            make
            make -j1 DESTDIR="$$INSTALL_ROOT" install
        '
        make -np > "$$EXEC_ROOT/$(location make-db.txt)" 2>/dev/null || true
        cd "$$EXEC_ROOT"
        $(location //tools:convert-element-autotools) \
            --trace="$$AUTOTOOLS_TRACE" \
            --make-db="$(location make-db.txt)" \
            --out-build="$(location BUILD.bazel.out)" \
            --out-install-mapping="$(location install-mapping.json)"
        tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
            -cf "$(location install_tree.tar)" -C "$$INSTALL_ROOT" .
    """,
    tools = [
        "//tools:build-tracer",
        "//tools:convert-element-autotools",
    ],
)
```

Single genrule, four outputs. The trace is captured to a transient
`$AUTOTOOLS_TRACE` path (mktemp) and fed to the converter inline —
it's not a Bazel-visible output, just an action-local sidecar
between the tracer and the converter. There is no separate
`<elem>_converted` target for `kind:autotools` — the convert step
is folded into the install action because trace + make-db aren't
byte-stable enough across runs to make a sibling rule's narrow
cache key actually hit. See `docs/trace-driven-autotools.md` for
the rationale and the roadmap to re-introduce the split once
trace normalization lands.

The tracer + converter run inside the same Bazel action — one
action, two outputs (install_tree.tar + BUILD.bazel.out). Bazel's
action cache (buildbarn in CI) handles cross-node convergence
automatically; same source + same toolchain + same converter
version => same outputs.

When the element has cross-element deps, an `imports.json` is also
rendered + listed in srcs + threaded through
`--imports-manifest=$(location imports.json)`, mirroring the
kind:cmake handler's contract.

#### Pipeline kinds without trace conversion (kind:make / kind:manual / kind:script)

Same shape as autotools' coarse path: a single
`<elem>_install` genrule whose `outs = ["install_tree.tar"]`. No
`BUILD.bazel.out`; downstream consumers reference the install
tree directly via filegroup composition (kind:stack /
kind:compose / kind:filter / kind:import).

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
| `//elements/<name>:<name>_install` | Install genrule. For pipeline kinds (kind:autotools / make / manual / script) this is the action that builds + installs + (for trace-driven kind:autotools) converts in one go. Outputs include `install_tree.tar`; trace-driven kind:autotools also emits `trace.log` / `make-db.txt` / `BUILD.bazel.out` / `install-mapping.json`. |
| `//elements/<name>:<name>_converted` | The cmake convert genrule (kind:cmake only). Outputs `BUILD.bazel.out` + `read_paths.json` + `cmake-config-bundle.tar`. Trace-driven kind:autotools folds convert into `_install`; there is no separate `_converted` rule. |
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
7. **Convergence via Bazel's action cache.** No tool-specific
   srckey registry; rely on Bazel's existing remote-cache
   plumbing for cross-node consistency. Action keys derive from
   inputs + tool digests; same source + same toolchain + same
   tool version → same outputs.

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
