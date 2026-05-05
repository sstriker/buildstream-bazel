# Whole-project plan: Bazel-as-orchestrator (two-pass meta-project)

The directional choice this repo makes: **keep the orchestrator small
and dumb; defer cross-element scheduling, action caching, and
dataflow to Bazel as a proven graph system**. Per-kind translators
are the unit of work, hosted as tools that per-element genrules
invoke inside a meta Bazel workspace.

## Goal

`bazel build //...` over a converted BuildStream project succeeds and
produces the same artifacts BuildStream would have. Every element
kind has a translator that emits BUILD.bazel rules. No runtime
dependency on `bst` for the Bazel build, and no orchestrator-side
scheduling or action cache — Bazel owns the cross-element graph.

## Why two-pass

BuildStream itself is a thin orchestrator over per-kind plugins
(`cmake`, `autotools`, `meson`, `manual`, `filter`, `stack`, …).
Each plugin interprets its element's `config:` block and produces
build outputs. The previous draft of this plan rebuilt a per-kind
dispatch table inside a Go orchestrator that also owned an action
cache, REAPI executor abstraction, regression-diff machinery, and
cross-element scheduling. Most of that machinery duplicates what
Bazel already provides.

The two-pass shape replaces it. Project A is a meta Bazel workspace
whose only purpose is converting elements: one `genrule` per element
that invokes the per-kind translator binary (`convert-element` for
`kind: cmake`, similar for the others) and produces BUILD.bazel +
typed export filegroups for that element. Project B is the
materialized union of project A's outputs plus the original sources;
it's a normal Bazel workspace consumers see and build against.
Bazel's action cache decides what to re-run in A; Bazel's normal
build machinery handles project B.

## Phase 0: survey FDSDK

Done — see `docs/fdsdk-element-survey.md`. Key takeaways shaping the
phasing below:

- 1 092 elements across 21 kinds. The original 7-kind enumeration
  (cmake, meson, autotools, manual, stack, filter, junction)
  covered 67 % of FDSDK; the other 33 % falls into 14 unlisted
  kinds that group cleanly into "buildsystem variants" (fold into
  the manual-shaped translator), "install-tree manipulation"
  (trivial filegroup emitters), and "FDSDK manifest glue" (small,
  plumb in last).
- `git_repo` (530 / 870 top-level sources) is the dominant source
  kind, not `git`. The sourcecheckout layer needs a small
  alias-resolving adapter for FDSDK's `include/_private/aliases.yml`.
- Per-kind config surface is small: `<kind>-local` (`cmake-local`,
  `meson-local`, `conf-local`, `autogen`) is the dominant per-element
  customization point and maps directly to flags appended to the
  plugin's default invocation. `command-subdir` recurs across
  kinds; translators must honor it.

The survey is a tool, not a deliverable; re-run it against newer
FDSDK snapshots when the percentages or kind set shifts.

## Architecture

### Project A and project B

Project A is the meta workspace:

```
project-A/
  MODULE.bazel             ← uses the writer-of-A module extension
  BUILD.bazel              ← (top-level, mostly empty)
  # Generated repos, one per .bst element:
  external/+_writer+fmt/
    BUILD.bazel            ← genrule(name="fmt", cmd="convert-element ...")
                              filegroup(name="cmake_config", ...)
                              filegroup(name="headers", ...)
                              filegroup(name="libs", ...)
  external/+_writer+glibc/
    BUILD.bazel            ← genrule(name="glibc", cmd="<manual install>")
                              filegroup(name="cmake_config", ...)
                              ...
```

Project B is materialized from project A's outputs plus the original
source tree:

```
project-B/
  MODULE.bazel             ← bazel_dep on each generated element
  elements/components/fmt/
    BUILD.bazel            ← (symlink/copy from project A's output)
    cmake-config/          ← (likewise)
    src/, include/, ...    ← (symlinks into the original source tree)
```

A user-facing wrapper script does `bazel build //... --output_base=A`
followed by `bazel build //... --output_base=B`, or a repo rule in B
chains them automatically. Two passes is more visible than today's
single orchestrator invocation; it's the trade for collapsing the
orchestrator's bespoke machinery.

### Per-element generated BUILD: `srcs = glob() + zero_files`

Each element's generated BUILD declares the element's convert-element
genrule with `srcs` composed of two parts: a `glob()` over the real
source paths the translator actually reads, and a `zero_files`
generated stub set for the rest of the universe cmake's
`file(GLOB)` walks would naturally see.

```starlark
load("//rules:zero_files.bzl", "zero_files")

zero_files(name = "fmt_stubs", paths = STUB_PATHS)

genrule(
    name = "fmt_converted",
    srcs = REAL_PATHS + [":fmt_stubs", "@deps//cmake-config:bundle"],
    outs = ["BUILD.bazel", "cmake-config/", "read_paths.json"],
    cmd  = "$(location //tools:convert-element) --kind=cmake ...",
    tools = ["//tools:convert-element"],
)
```

The `zero_files` rule is ~10 lines of starlark using
`ctx.actions.write(output, content="")`. Both motivations land:
cmake's `file(GLOB)` walks see the directory shape unchanged
(content is irrelevant for files cmake doesn't open), and the action
input merkle is content-stable across edits to non-read files.

### Read-set narrowing via `read_paths.json` feedback

The set `REAL_PATHS` for each element is determined by the
converter's `read_paths.json` output from the previous successful
conversion. First-run is uncached (full source set is real, nothing
zeroed); subsequent runs use the committed `read_paths.json` to
narrow.

```
universe   = glob(["**/*"])
read_set   = json.decode(rctx.read("read_paths.json"))
real_paths = [p for p in universe if p in read_set]
stub_paths = [p for p in universe if p not in read_set]
```

`read_paths.json` lives alongside each .bst element and is committed
when the user accepts the new read set. Bazel's action cache keys on
the merkle of (real reads + zero stubs), so semantically-irrelevant
edits to non-read files don't invalidate the action.

### Cross-element inputs: typed filegroups + per-consumer narrowing

Each element exports typed filegroups consumers reference:

```starlark
filegroup(name = "cmake_config", srcs = glob(["install/lib/cmake/**"]))
filegroup(name = "pkg_config",   srcs = glob(["install/lib/pkgconfig/*.pc"]))
filegroup(name = "headers",      srcs = glob(["install/include/**"]))
filegroup(name = "libs",         srcs = glob(["install/lib/lib*.{so*,a}"]))
filegroup(name = "binaries",     srcs = glob(["install/bin/*"]))
```

Consumers reference deps' typed slices: a downstream cmake element
has `srcs = ["@dep//cmake-config:bundle", "@dep//headers:public"]`.
For deep deps where the consumer reads only some headers (typical
for glibc, openssl, ...), per-consumer narrowing applies the same
zero-stub trick: `@dep//headers:reads_for_<consumer>` exposes only
the consumer's read subset, with stubs covering the rest. Today's
`synthprefix.Build()` does element-level staging without this
narrowing; the meta-project shape gives strictly stronger
cross-element cache stability.

The path-anchor wrinkle: today the converter sees the prefix at
`/opt/prefix/...` (`manifestPrefixAnchor`), but Bazel stages dep
bundles at label paths. Each element genrule's `cmd` does an
in-action symlink-merge of N dep bundles into `$SYNTH_PREFIX`
before invoking the translator — five lines of bash. The
translator binaries stay unchanged.

### Output shape for non-cmake kinds

Coarse-grained kinds (autotools, manual, make, pyproject, …)
produce an install tree as a tree artifact (`ctx.actions.declare_directory`).
A wrapping rule exposes typed slices via providers
(`HeadersInfo`, `LibsInfo`, `PkgConfigInfo`, …); consumers depend
on the typed slices, not the raw tree. Internally one
content-addressed install tree per element; externally normal
Bazel rules. Caching comes from the tree's merkle; consumer
narrowing comes from filegroup/provider plumbing on top.

For elements that already declare their output shape in the .bst
(`flatpak_image`'s `directory:` and `metadata:`, `compose`'s
`exclude:`), the writer-of-A renders explicit `outs = [...]`
patterns directly. Tree artifacts are only needed where the .bst
doesn't pre-declare shape.

### Toolchain bootstrap

The toolchain element (gcc + glibc + binutils, currently
`kind: manual` in FDSDK) produces an install tree via its
genrule. A starlark macro — invoked from the writer-of-A when the
.bst is tagged as a toolchain provider — synthesizes a
`cc_toolchain_config.bzl` from the install tree's binary list and
sysroot layout. Downstream cmake-element genrules pick it up via
Bazel's normal `cc_toolchain` resolution.

The bootstrap chain (host-toolchain builds toolchain-stage1 builds
toolchain-stage2 builds …) becomes an honest Bazel dependency
graph: each stage's tree depends on the previous one. Today's
`derive-toolchain` binary collapses into one starlark macro the
writer-of-A invokes at .bst-parse time.

### Writer-of-A: the minimum viable orchestrator

Small Go binary (target ~500 LOC) plus a starlark module extension
that calls it. Inputs: the .bst graph + project.conf. Outputs:
project A's `MODULE.bazel`, a generated repo per element with the
right BUILD.bazel, and the inputs to project B (a `MODULE.bazel`
declaring the per-element `bazel_dep`s).

It does NOT: run actions, manage caches, schedule cross-element
work, or talk to REAPI. All of that lives in Bazel.

It DOES: parse .bst (YAML + BST directives), resolve sources via
the existing `sourcecheckout` package, render starlark templates
for each element kind, render the imports manifest as JSON
alongside each element, and chain through Bazel's module-extension
caching so re-runs are cheap.

## Per-kind translation strategies

Strategies organized by how the kind lowers in project A's BUILD:

### `kind: cmake`

**Pattern**: per-element genrule invoking `convert-element --kind=cmake`.
Outputs: `BUILD.bazel`, `cmake-config/` bundle, `read_paths.json`.
Reuses the existing `convert-element` binary unchanged. The starlark
template renders the genrule's `cmd` with the element's
`cmake-local` flags inline.

### `kind: meson`

**Pattern**: per-element genrule invoking `convert-element --kind=meson`
(new translator binary, similar shape — runs `meson setup` +
`meson introspect`, lowers to the same IR / emit pipeline as cmake).

### `kind: autotools` / `make` / `manual` / `pyproject` / `script` / `makemaker` / `modulebuild`

**Pattern**: coarse-grained install pipeline. Per-element genrule
runs `configure` (where applicable) + `make` + `make install`
inside Bazel's action sandbox, producing a tree artifact install
dir. A wrapping rule exposes typed filegroups. The .bst's
`config: <phase>-commands` and `variables:` get rendered into the
genrule's `cmd` by the writer-of-A.

The buildsystem variants (`make`, `pyproject`, `makemaker`,
`modulebuild`) all share the manual-shape pattern with different
defaults. One shared starlark macro plus a per-kind defaults dict.

### `kind: stack` / `filter` / `import` / `compose` / `flatpak_image` / `snap_image` / `flatpak_repo` / `collect_*` / `check_forbidden`

**Pattern**: pure starlark filegroup composition. No action runs;
the writer-of-A emits `filegroup`s with `srcs` referencing parent
elements' typed slices. Trivial.

### `kind: junction`

**Pattern**: handled at writer-of-A time, not rule-emission. A
junction's target project gets parsed by a separate writer-of-A
invocation rooted at the junction's source dir; its generated
elements live under a different bazel_dep chain in project A's
MODULE.bazel.

### Source kinds

Independent of element kind. Already partially handled by
`orchestrator/internal/sourcecheckout`. Per source kind:

- `git_repo` (530, dominant): URL-alias-resolving variant of `git`.
  Needs a small adapter for FDSDK's `include/_private/aliases.yml`
  (parse aliases, rewrite URLs at fetch time).
- `git`, `tar`, `local`, `remote`, `patch`: handled.
- `go_module`, `cargo2`, `cpan`, `pypi`, `zip`: language-package
  source kinds with vendored ref lists. Handled by the language-
  element translators that need them; orthogonal to the
  source-checkout core.

## Phasing

Phases 2 and 3 can interleave once Phase 1's gate is green; Phase
4 sequences last.

**Phase 1 — writer-of-A + cmake elements only.** *(delivered)*
`cmd/write-a` Go binary, `rules/zero_files.bzl` Starlark rule,
`testdata/meta-project/hello-world.bst` fixture, and
`scripts/meta-hello.sh` driving `make e2e-meta-hello`. The
existing `orchestrator/cmd/orchestrate` continues to work
alongside as the fallback path during transition. The gate
exercises:

  - Hello-world round-trip: writer-of-A renders project A and
    project B; bazel build A invokes convert-element via genrule;
    bazel build B compiles a smoke `cc_binary` against the
    converted `cc_library` and runs it.
  - Cache-stability scenario A (edit a `.c` source file NOT in
    cmake's read set): convert-element cache-hits in A; smoke
    binary still runs in B. The `zero_files`-backed input merkle
    is content-stable across edits to non-read files.
  - Cache-stability scenario B (edit a CMakeLists.txt comment IS
    in the read set): convert-element re-runs in A but produces a
    byte-identical `BUILD.bazel.out`; B's smoke binary sha is
    unchanged. cmake's parser strips comments before the codemodel.

This is the gate for committing to delete the old orchestrator
machinery; with the meta-project shape proven on hello-world,
later phases expand the kind set and graph shape.

**Phase 2 — install-tree-manipulation kinds.** *(in progress)*
stack, filter, import, compose, flatpak_image, snap_image,
flatpak_repo, collect_manifest, collect_initial_scripts,
collect_integration, check_forbidden. All are pure starlark
filegroup composition; no action runs, no new translator
binaries. ~13 % of FDSDK in this bucket.

The first cut delivers the framework — multi-element graph
parsing, the `kindHandler` per-kind dispatch interface, and
`kind: stack` — gated by `make e2e-meta-stack` against
`testdata/meta-project/two-libs/` (2 cmake elements + 1 stack
bundling them; smoke `cc_binary` linking against both via the
stack's filegroup).

`kind: compose`, `kind: filter`, and `kind: import` follow on the
same dispatch shape. `kind: compose` is rendering-equivalent to
`kind: stack` with conflict detection deferred to Bazel's own
filegroup-collision reporting. `kind: filter` enforces the
single-dep invariant and decodes `config: include / exclude /
include-orphans` into BUILD comments — domain-based slicing
itself reshapes into per-dep typed-filegroup labels and lands
alongside the typed-filegroup wrapper for pipeline-kind outputs.
`kind: import` stages a `kind: local` source tree verbatim into
project B and exposes it as `filegroup(srcs = glob([...]))`.
Gated by `make e2e-meta-compose` / `e2e-meta-filter` /
`e2e-meta-import`.

The remaining kinds in this bucket (`flatpak_image`, `snap_image`,
`flatpak_repo`, `collect_*`, `check_forbidden`) reshape into the
same filegroup-composition shape with kind-specific config
parsing; they land in follow-up PRs once their cross-element
semantics need real install-tree producers (see Phase 3) and
typed filegroups.

**Phase 3 — buildsystem-variant kinds.** *(in progress)* meson,
autotools, make, manual, pyproject, makemaker, modulebuild,
script. The fine-grained meson translator is its own genrule
shape; the coarse ones share the install-pipeline pattern.
Together with cmake this covers ~70 % of FDSDK by element count.

The first cut delivers `kind: manual` — the simplest of the
coarse-grained kinds — gated by `make e2e-meta-manual` against
`testdata/meta-project/manual-greet/`. Validates the per-element
genrule shape and the four phase command lists (configure / build /
install / strip).

A shared `pipelineHandler` shape extracted from kind:manual lets
sibling coarse-grained kinds register as small `init()` blocks
with their default phase commands. `kind: make` follows, with
BuildStream's `make` plugin defaults declared via per-kind
variable bindings (`%{make-args}` / `%{make-install-args}`); gated
by `make e2e-meta-make` against `testdata/meta-project/make-greet/`
(a Makefile-driven binary build that exercises the default-commands
path end-to-end).

The variable resolver (`cmd/write-a/variables.go`) layers four
scopes, lowest precedence first: BuildStream stock defaults
(`projectVars`, mirroring `buildstream/data/projectconfig.yaml`'s
`prefix=/usr/local` baseline), the meta-project's `project.conf`
`variables:` block (`cmd/write-a/project_conf.go`, walked up from
each `.bst`), the per-kind defaults the handler registers, and the
per-element `variables:` block in the `.bst`. References resolve
recursively with cycle detection; `%{install-root}` /
`%{build-root}` are runtime sentinels mapped to the genrule cmd's
exported shell vars. Gated by `make e2e-meta-vars` against
`testdata/meta-project/vars-greet/` (`prefix=/usr` from
project.conf, then `prefix=/opt/freedesktop-sdk` from element
variables — exercising the precedence chain end-to-end).

With the resolver in place, the sibling pipeline kinds collapse to
small per-kind registration files — each declaring its plugin's
default variables and command templates. `kind: autotools` lands
this way: a pipelineHandler registration with BuildStream's
canonical `%{autogen}` / `%{configure}` / `%{make}` /
`%{make-install}` chain (autogen detects autogen / autogen.sh /
bootstrap / autoreconf, then `./configure` runs with the canonical
`--prefix` / `--bindir` / `--libdir` / ... flag set sourced from
the resolved variable scope). Gated by `make e2e-meta-autotools`
against `testdata/meta-project/autotools-greet/` (a minimal
`./configure` script plus `Makefile.in` template plus C source —
exercises the configure → make → install phase chain end-to-end
under the project.conf prefix override). `pyproject`, `makemaker`,
`modulebuild`, and `script` follow the same shape; each is a small
follow-up. `meson` is a separate fine-grained track.

cc_toolchain integration deferred. `kind: autotools` (and every
other pipeline kind today) inherits whatever C compiler is on the
exec sandbox's PATH. Routing `%{cc}` / `%{cflags}` / `%{ldflags}` /
`%{strip}` through Bazel's `cc_toolchain` (via `toolchains = [...]`
+ `$(CC)` make-vars piped into the genrule cmd's exported `CC` /
`CFLAGS`) is shared work for every pipeline kind, not autotools-
specific; lands once host-toolchain inheritance stops being
sufficient for FDSDK reality-check.

**Phase 4 — FDSDK acceptance.** Run the full pipeline over the
FDSDK kind set the survey covers. `bazel build //...` against
project B succeeds. Document remaining deltas in
`docs/fidelity-known-deltas.md`. After this, the gate moves to
the full FDSDK graph; the old orchestrator code can be deleted.

The first reality check against real FDSDK
(`docs/fdsdk-reality-check.md`, runnable via
`make fdsdk-reality-check`) catalogs the loader / cross-element
resolution / composition-directive gaps a Phase-4 attempt hits
today. The smallest unblocker chain is `build-depends` /
`runtime-depends` parsing → path-qualified element resolution →
multi-source elements + `public:` tolerance + per-source
`directory:`. After those three the survey re-runs from the
variable-resolver side; the next forcing function is `(@):`
composition (a YAML pre-processor at write-a's loader), and the
architectural piece — `(?):` conditionals lowering to project-B
Starlark `select()` over `@platforms` — lands once a multi-arch
fixture forces it.

## Out of scope (explicitly)

- **Replacing every element kind's plugin with a fine-grained
  translator on day one.** Coarse-grained is acceptable as a
  default for kinds where introspection isn't natively available.
- **Reimplementing BuildStream's plugin model in Go.** The
  translators borrow the *interface idea* but each one is a small
  focused emitter, not a faithful re-implementation of the BST
  plugin.
- **Workspace-aware translation** (handing converted BUILD.bazel
  files back into a `bst workspace`-driven dev loop). Not a v1
  concern.

## Open questions

1. **Project A → project B materialization.** Two options: a
   wrapper script (`make convert && bazel build //...`) or a
   repo rule in B that triggers project A as a sub-build. The
   former is simpler; the latter composes better with downstream
   `bazel_dep`. Phase 1 picked the wrapper-script path
   (`scripts/meta-hello.sh`); the repo-rule alternative is
   re-evaluated when graphs grow past one element.
2. **Where `read_paths.json` lives.** Committed alongside each
   .bst (the orchestrator's allowlistreg-on-disk pattern today),
   or generated as a project-A action output and consumed by a
   subsequent module-extension regeneration. Committed-alongside
   is simpler; revisit if churn is annoying in practice.
3. **`derive-toolchain` migration timing.** Phase 1 uses the
   host's `cc_toolchain` to keep the writer-of-A small. The
   toolchain-bootstrap macro lands in Phase 3 alongside
   `kind: manual`. Phase 4's FDSDK gate forces the migration.
