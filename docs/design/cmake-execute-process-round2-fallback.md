# `kind:cmake` round-2 fallback for unliftable `execute_process`

Phase A of the `execute_process` recovery (PR #95) emits a typed
`unsupported-execute-process` Tier-1 failure for calls the
classifier can't lift natively (`stamp` / `probe` / `unknown`
buckets — `git rev-parse`, `uname -m`, opaque pipelines, etc.).
Today, those failures mark the element as excluded by the
orchestrator. Phase B's goal: replace the exclusion with a
**round-2-style coarse-genrule fallback** that still produces a
buildable downstream artifact, mirroring how
`kind:autotools / make / makemaker / modulebuild / manual /
script` handle elements they can't fully translate.

This doc covers the architectural shape of that fallback and
the staged implementation plan. The kind-agnostic rendezvous
mechanism is unchanged from `docs/design/autotools-round2-rendezvous.md`;
only the kind-specific wiring is new.

## The architectural mismatch

The pipeline kinds (`kind:make / makemaker / modulebuild / manual /
script`) joined round-2 via a one-line opt-in: each one is a
`pipelineHandler` variant (`cmd/write-a/handler_pipeline.go:51`),
and round-2 dispatch hangs off the `traceDrivenSrckeyPatterns`
field (`handler_pipeline.go:85`). When the field is non-nil and
runtime config enables round-2,
`pipelineHandler.shouldUseRound2()` returns true and `RenderA` /
`RenderB` route through the kind-agnostic helpers in
`handler_pipeline_round2.go`.

`kind:autotools` joined round-2 too, but via a different
mechanism: `autotoolsHandler` (`handler_autotools_native.go`) is
its own struct, not a `pipelineHandler` variant. Its `RenderA`
calls `renderTraceDrivenRound2A` (the kind-agnostic helper)
directly when `autotoolsConfig.round2Enabled` is set, and its
`RenderB` produces the install genrule via the
`pipelineExtension` returned by `pipelineTraceExtensionRound2`.
Same destination, different dispatch — but still bounded
(autotools has a single config flag, not per-element gating).

`kind:cmake` is neither shape. `cmakeHandler` is its own
`struct{}` (`handler_cmake.go:39`) with bespoke `RenderA` /
`RenderB` that don't go through either dispatcher. The round-1 /
round-2 split there isn't a configure-then-make-then-install
shape; it's a single per-element genrule that runs
`convert-element` against cmake's File API reply. There's no
`traceDrivenSrckeyPatterns` field to set; no `shouldUseRound2`
branch to flip; and no autotools-style `round2Enabled` config
that would route every element through round-2 either.

So Phase B can't be a one-line opt-in. The choice is between:

1. Refactor `cmakeHandler` to be a `pipelineHandler` variant,
   then opt in.
2. Keep `cmakeHandler` separate and add a parallel round-2
   render path inside it.

Option (2) is what this doc proposes: **`kind:cmake` keeps its
native render as the primary path, and the round-2 shape is a
per-element fallback that activates when convert-element exits
with `unsupported-execute-process`**. Option (1) would force
every `kind:cmake` element through the round-2 path
unconditionally (the pipeline-kinds and autotools opt-ins are
both build-wide, not per-element), which sacrifices native
render's fine-grained `cc_library` / `cc_binary` outputs for
projects that don't need the fallback.

## Per-element fallback decision

write-a stays uniform across kind:cmake elements: every
element gets the same converter-genrule shape, with the same
output set. **convert-element decides per-element at action
time** whether to take the native render path or the fallback
path, based on whether classification refuses any
`execute_process` calls. write-a never needs to know which
elements opt into fallback — the per-element decision is
fully encapsulated in convert-element.

This avoids three uglier alternatives:

- **write-a runs cmake configure per element.** Eliminates the
  decision-time problem at the cost of polluting write-a's
  wallclock boundary. Rejected — write-a is a parsing +
  emission pass; running cmake at write-a time ties its
  latency to every cmake project's configure cost.
- **Per-element opt-in in `.bst`.** Operators tag elements
  `cmake.round2: true` after seeing failures. Rejected —
  duplicates information already in the `failure.json` triage
  report; encourages the manifest and the failure to drift
  apart.
- **Separate Project B install genrule.** Mirrors
  kind:autotools round-2 (where the install genrule lives in
  Project B). For kind:cmake this would force write-a to
  emit two genrules per element (converter in A, install in
  B). Rejected — bolts the round-2 install steps onto
  convert-element's existing genrule keeps the per-element
  decision in one place; convert-element already runs cmake
  configure, so adding "and conditionally also ninja +
  install" is a smaller delta than wiring a parallel B-side
  genrule.

The chosen shape: convert-element's genrule has a fixed
output set (`BUILD.bazel.out`, `install_tree.tar`,
`trace.log`, `failure.json`). On native success
install_tree.tar is empty; on fallback it contains the
artifacts. write-a doesn't observe the choice; operators see
which path was taken via the `failure.json` triage report
and a stderr banner from convert-element.

## The shape

```
write-a (no kind:cmake-side opt-in needed)  →
  project A: per-element converter genrule
             outs: [BUILD.bazel.out, install_tree.tar,
                    trace.log, failure.json]
  project B: thin source-staging stub (unchanged from today)

bazel build A//<elem>:<elem>_build
   converter genrule (one action covers both render paths):
     cmake configure + File API reply + trace
     convert-element runs lower.ToIR(...)
       → success            ⇒ emit fine-grained cc_library / cc_binary
       → unsupported-execute-process
                            ⇒ emit placeholder BUILD.bazel.out:
                                aliases pointing at <elem>_install_tree

bazel build B//<elem>:<elem>_install
   pass-3 install genrule:
     build-tracer wraps:
       cmake -B build [...]
       cmake --build build
       cmake --install build --prefix=$INSTALL_ROOT
     synthesize empty make-db.txt (cmake has no make-db; the
       trace-publish wire contract requires the slot)
     trace-publish (defense-in-depth, lands AC entry)
```

Native render and fallback share **one** convert-element
genrule. The per-element decision is fully encapsulated; the
genrule's output set is uniform (so write-a renders one
template); convert-element fills in whatever subset of those
outputs the chosen path needs.

### Why one genrule, not two

The `kind:autotools` round-2 architecture splits the work
across Project A (converter genrule) and Project B (install
genrule). For autotools that split is forced — autotools has
no analysis-time introspection so the converter can't run
without the install actually happening, and the natural place
for the install is Project B's existing build action.

`kind:cmake` doesn't have that constraint. cmake's File API
gives the converter analysis-time introspection; the
converter already runs cmake configure. Bolting "and
optionally also `ninja` + `cmake --install`" onto the same
action keeps the per-element decision in one place and avoids
write-a needing to know which elements opt into fallback.

## Convert-element's failure → placeholder transition

Today, `convert-element` exits 1 on `unsupported-execute-process`
and the genrule action fails. For the fallback, the genrule
action must succeed even when classification refuses, run
cmake build + install instead of giving up, and emit a
placeholder BUILD whose per-target stubs delegate to the
fresh install_tree.tar.

Implementation: a new `--unsupported-execute-process-fallback`
flag on `convert-element`. When set:

- Classifier's refusal path no longer returns Tier-1; instead
  `recoverExecuteProcess` returns a structured refusal slice
  the lowering reads.
- `lower.ToIR` calls `emitFallbackPlaceholder` to produce an
  `ir.Package` of per-target stubs (shape described below).
- (Future Step 3) convert-element ALSO runs `cmake --build` +
  `cmake --install` + `tar` to populate install_tree.tar in
  the same action, and writes failure.json with the per-call
  triage report. Step 2 / Step 2.5 just emit the BUILD shape;
  the actual build-and-install work in convert-element lands
  in Step 3.

### Placeholder shape (per-target rules from install destinations)

Aliasing every cmake target to the coarse `install_tree.tar`
doesn't work — Bazel can't link a tar archive into a
downstream `cc_binary`. Downstream consumers expect the same
ergonomics native render gives them: `deps = [":thelib"]`
should pull in linkable artifacts.

The codemodel records `Target.Install.Destinations[].Path` per
target plus `Target.NameOnDisk` (the artifact filename).
Combined, that's enough to reconstruct each target's path
inside the install_tree without parsing tar contents at
action time. `emitFallbackPlaceholder`
(`converter/internal/lower/execute_process_fallback.go`) walks
the codemodel and dispatches on `Target.Type`:

```python
# One-time tar extract genrule. Outputs are enumerated from
# Target.Install.Destinations + NameOnDisk; the genrule's src
# is the literal "install_tree.tar" label that write-a (Step
# 3) wires to the converter genrule's own install_tree.tar
# output (or wherever the install bytes come from).
genrule(
    name = "_install_tree_extract",
    srcs = ["install_tree.tar"],
    outs = [
        "install_tree/lib/libthelib.a",
        "install_tree/lib/libshared.so.1",
        "install_tree/bin/thetool",
        # ...one entry per (target, install destination).
    ],
    cmd = """mkdir -p "$(@D)/install_tree" && \
tar -C "$(@D)/install_tree" -xf "$(location install_tree.tar)"""",
    tags = [
        "cmake-codegen-execute-process-fallback",
        "cmake-codegen-execute-process-fallback-extract",
    ],
    visibility = ["//visibility:private"],
)

# Per-target stub rules. Kind dispatch follows Target.Type:
cc_import(
    name = "thelib",                   # STATIC_LIBRARY / OBJECT_LIBRARY
    static_library = "install_tree/lib/libthelib.a",
    tags = ["cmake-codegen-execute-process-fallback"],
    visibility = ["//visibility:public"],
)
cc_import(
    name = "shared",                   # SHARED_LIBRARY / MODULE_LIBRARY
    shared_library = "install_tree/lib/libshared.so.1",
    tags = ["cmake-codegen-execute-process-fallback"],
    visibility = ["//visibility:public"],
)
sh_binary(
    name = "thetool",                  # EXECUTABLE
    srcs = ["install_tree/bin/thetool"],
    tags = ["cmake-codegen-execute-process-fallback"],
    visibility = ["//visibility:public"],
)
cc_library(
    name = "headers_only",             # INTERFACE_LIBRARY (header-only)
    tags = ["cmake-codegen-execute-process-fallback"],
    visibility = ["//visibility:public"],
)
```

Targets without an `Install` block — utility, internal-only
libraries, the project's private build artefacts — are
**omitted** from the placeholder. Downstream consumers
referencing such labels get a Bazel "label not found" error
that's a clear signal: the target wasn't part of the install
contract; either expose it via `install()` upstream or stop
depending on it across the round-2 boundary.

### Header wiring (deferred)

`cc_import.hdrs` and `cc_library.hdrs` are not populated by
the v1 placeholder. The codemodel's `Target.FileSets` records
header sets per target with their install destinations; an
analogous extract step could enumerate them as additional
extract-genrule outs and feed them as `hdrs` on the per-target
stubs. Deferred until a real fixture forces it; without
hdrs, downstream `#include <thelib.h>` won't resolve through
the placeholder, but the `deps` linkage still works for
linkable artifacts.

### Why not filegroups everywhere?

A simpler shape — emit a `filegroup(srcs = ["lib/libthelib.a"])`
per target — wouldn't satisfy `cc_binary`'s `deps` interface
(filegroups don't carry the `CcInfo` provider). `cc_import`
gets the linkable `CcInfo` automatically. The placeholder
uses `filegroup` only for the few target types that don't fit
any cc_* rule.

## The kind-specific bits

What's new for `kind:cmake` Phase B (everything else reuses the
kind-agnostic infra in `handler_pipeline_round2.go`):

- **`cmakeSrckeyPatterns()`** — the file-glob set that gates
  what counts as content-included for srckey computation.
  Contents (see the function's doc comment for the per-glob
  rationale):
  - `CMakeLists.txt`, `**/CMakeLists.txt`, `*.cmake`,
    `**/*.cmake`, `*.cmake.in`, `**/*.cmake.in`
    (cmake-driven build commands).
  - `**/*.h` family + `**/*.hpp` + `**/*.hxx` + `**/*.hh`
    (header changes can shift include resolution, which
    surfaces in the trace).
  - `CMakePresets.json` / `CMakeUserPresets.json`
    (alternative configure entry points).
  - **`.h.in` is path-only by default** — cmake reads them at
    configure time (`RERUN_CMAKE` flags them) but the
    configure_file lift in PR #94 makes them Bazel-srcs
    covered, removing the need for srckey content-inclusion.
    Elements without the lift staged surface undercoverage
    drift in `audit-narrowing` (cmake oracle reports `.h.in`,
    patterns don't cover); operators react by either staging
    the lift OR adding a per-element `include **/*.h.in`
    override to read-paths.txt. The audit's report is the
    warning channel — there's no need for a bespoke
    "did-you-mean-to-stage-the-lift" check.
  - Compile sources (`*.c` / `*.cc` / etc.) **path-only** —
    the trace records compile commands, not source bytes;
    edits to `.c` files don't change the trace's structure.
- **`wrapCmakePipelineCmds()`** — mirrors
  `wrapAutotoolsPipelineCmds()`
  (`handler_autotools_native.go:386`). Wraps cmake's
  `configure / build / install` sequence under build-tracer
  with `--normalize-prefix` for byte-stable traces.
  - Configure: `cmake -B "$$BUILD_ROOT" -G Ninja -S "$$SRC_DIR" -DCMAKE_INSTALL_PREFIX="$$INSTALL_ROOT" [...]`
  - Build: `cmake --build "$$BUILD_ROOT" --parallel 1`
  - Install: `cmake --install "$$BUILD_ROOT" --prefix "$$INSTALL_ROOT"`
  The `--parallel 1` matches `make -j1` in autotools round-2
  for trace stability (no interleaved subprocess output).
- **A render gate** at `scripts/meta-cmake-round2-fallback.sh`,
  modeled on `meta-autotools-round2.sh` but asserting the
  cmake-specific shape (placeholder BUILD.bazel.out content,
  cmake-flavoured install genrule cmd).
- **A live-AC gate.** The existing
  `tools/e2e-meta-autotools-round2-live.sh` is mixed: the
  publish/lookup wire contract it round-trips through a real
  REAPI endpoint is genuinely kind-agnostic (any kind using
  `rules/traces.bzl`'s `_trace_repo` hits the same
  `SyntheticActionDigest(srckey)` path), but the bazel-build
  half hard-codes the autotools fixture + binary +
  `//elements/greet:greet_build` target. So `kind:cmake` needs
  one of:
  1. A dedicated cmake live gate (analogue of the autotools
     one) using a kind:cmake fixture and the cmake-side
     `bazel build //elements/<demo>:` target.
  2. A refactor of the existing live gate to take fixture +
     converter binary + target label as arguments and run once
     per kind.
  v1 picks (1) — bounded and concrete; (2) is a follow-up if
  more kinds join. The render-half acceptance still lives in
  `scripts/meta-cmake-round2-fallback.sh` and is the
  primary contract for write-a's emission shape.

## Staged implementation

Each step is a self-contained PR; later steps build on earlier
ones but each leaves the tree in a runnable state.

1. **`cmakeSrckeyPatterns()` + `wrapCmakePipelineCmds()`** as
   pure functions with unit tests. No call sites yet — pure
   scaffolding. Lets the bucket-of-globs / build-tracer
   wrapping be reviewed in isolation before any wiring.
2. **convert-element `--unsupported-execute-process-fallback`
   flag.** When set, classifier refusals stop exiting Tier-1;
   `lower.ToIR` returns a placeholder `ir.Package` (initially
   per-target empty stubs in PR #97; upgraded to full
   per-target `cc_import` / `sh_binary` / extract-genrule
   shape in Step 2.5 — PR #98). The flag is off by default;
   existing flows unchanged.
3. **write-a `--cmake-round2-fallback` flag + per-element
   round-2 install genrule emission.** When the flag is on,
   write-a emits the round-2 install genrule alongside the
   native converter genrule, and threads
   `--unsupported-execute-process-fallback` into the
   converter genrule's cmd. Render gate
   `scripts/meta-cmake-round2-fallback.sh` lands here.
4. **Roadmap promotion + docs.** Move the ROADMAP `Later`
   bullet to `Now`/`Done` as the steps land. Update
   `docs/research/cmake_analysis.md` §9 to reflect the
   shipped fallback path.

Each step ships with `go test ./...` clean, `gofmt -l .`
empty, and the relevant render/live-AC gate green.

## Trade-offs and known gaps

- **Doubled build graph for fallback-enabled elements.** Each
  element with fallback enabled gets two genrules (converter
  + install). The install genrule only builds when consumers
  pull on it, but the analysis-phase cost is doubled.
  Mitigation: the fallback flag is opt-in — projects without
  unliftable `execute_process` shouldn't enable it.
- **Trace publish overhead at first build.** The round-2
  install genrule runs cmake configure + ninja + install
  every first-time srckey, even when the native render
  succeeded. Mitigation: the install genrule is only built
  when the placeholder BUILD.bazel.out actually points at it,
  which only happens on classifier refusal.
- **Placeholder aliasing fidelity.** Downstream consumers
  reference fine-grained labels like `:foo_lib`. The
  placeholder must alias every such label to the install_tree
  contents. The exhaustive set of labels comes from cmake's
  codemodel, so the placeholder generator can enumerate them
  even when the rest of `lower.ToIR` refuses.
- **Compile-flag fidelity.** The round-2 install genrule
  builds the project with whatever cmake/ninja default flags
  the project's CMakeLists.txt selects. Downstream Bazel
  consumers can't override `-O3` vs `-O2` etc. — they get
  whatever the install genrule picked. Mitigation: round-2
  is the *fallback*, not the primary path. Native render
  still applies for projects without unliftable
  `execute_process`.
- **Fixture fragility.** Building the round-2 install genrule
  in tests requires a real cmake + ninja on the CI runner.
  The render-half acceptance gate
  (`scripts/meta-cmake-round2-fallback.sh`) is render-only — it
  only invokes write-a and asserts the emitted BUILD shape, no
  bazel build half — so it stays runnable without bazel on the
  host. The live-AC gate
  (`tools/e2e-meta-autotools-round2-live.sh`, the future
  cmake sibling) is the one that exercises a real `bazel
  build`; that gate skips its bazel half cleanly when Bazel is
  missing or its major version is < 9 (Bazel 9 toolchain
  expectations are part of the contract). Same convention is
  expected for the cmake live gate when it lands.

## Reference

| File | Role |
|---|---|
| `cmd/write-a/handler_cmake.go` | `cmakeHandler` — primary kind:cmake render |
| `cmd/write-a/handler_pipeline_round2.go` | kind-agnostic round-2 helpers (reused) |
| `cmd/write-a/handler_autotools_native.go:wrapAutotoolsPipelineCmds` | model for `wrapCmakePipelineCmds` |
| `converter/internal/lower/execute_process.go` | classifier refusal path; gains the fallback sentinel branch |
| `cmd/build-tracer/main.go` | already kind-agnostic; reused as-is |
| `cmd/trace-publish/main.go` | already kind-agnostic |
| `cmd/trace-lookup/main.go` | already kind-agnostic |
| `internal/tracenorm/synthkey.go` | `SyntheticActionDigest(srckey)` — already kind-agnostic |
| `scripts/meta-cmake-round2-fallback.sh` | new render gate (Step 3) |
| `tools/e2e-meta-autotools-round2-live.sh` | live-AC gate; publish/lookup wire half is kind-agnostic, but the bazel-build half is autotools-fixture-specific. A cmake sibling gate is a follow-up. |
