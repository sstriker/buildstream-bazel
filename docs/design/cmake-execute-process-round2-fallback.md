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

Three plausible places to make the "native vs fallback"
decision per element. We pick (b).

(a) **At write-a time.** write-a runs a pre-pass that invokes
    cmake configure + classification per element, learns which
    fail Phase A's classifier, and emits round-2 shape only
    for those. Cost: write-a becomes much more expensive (one
    cmake configure per element, sequentially). Rejected —
    write-a today is a parsing + emission pass; running cmake
    pollutes the boundary and ties write-a's wallclock to
    every cmake project's configure cost.

(b) **Inside the per-element converter genrule.** convert-element
    runs as today; on `unsupported-execute-process` it writes
    a *placeholder* `BUILD.bazel.out` that aliases the
    element's expected library / binary names to a
    sibling-emitted round-2 install_tree.tar. Both the native
    converter genrule and the round-2 install genrule are
    emitted per element. Cost: every `kind:cmake` element with
    fallback enabled gets a round-2 install genrule alongside
    its native converter genrule, doubling the build graph
    width. The install genrule is only *built* when downstream
    consumers reference it, which they only do when native
    fails — so wallclock cost is paid only on the elements
    that actually need the fallback.

(c) **Out-of-band manifest.** Operators see `unsupported-execute-process`
    failures, edit a manifest declaring those elements
    "round-2 only", re-run write-a. Cost: manual
    bookkeeping, but fully precise about which elements pay
    the cost. Rejected — duplicates information already in the
    failure.json output; encourages the manifest and the
    failure to drift apart.

## The shape

Almost identical to `kind:autotools` round-2:

```
write-a --cmake-round2-fallback  →
  project A: per-element converter genrule (unchanged from native)
             + per-element round-2-fallback alias group
  project B: per-element round-2 install genrule (build-tracer-wrapped
             cmake configure + ninja + install + trace-publish)

bazel build A//<elem>:<elem>_build
   pass-2 converter genrule:
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
       cmake --install build --prefix=$DESTDIR
     post-process make-db (sed filter)
     trace-publish (defense-in-depth, lands AC entry)
```

The native render is untouched — projects without
`execute_process` (or with only liftable buckets) keep
emitting `cc_library` / `cc_binary` rules. The fallback is
extra wiring that only kicks in on classifier refusal.

## Convert-element's failure → placeholder transition

Today, `convert-element` exits 1 on `unsupported-execute-process`
and the genrule action fails. For the fallback, the genrule
action must succeed even when classification refuses, but
emit a placeholder BUILD that delegates to the round-2
install_tree.tar.

Implementation: a new `--unsupported-execute-process=fallback`
flag on `convert-element`. When set:

- Classifier's refusal path no longer returns Tier-1; instead
  `recoverExecuteProcess` returns a sentinel value the lowering
  reads.
- `lower.ToIR` returns a *placeholder* `ir.Package` whose shape
  is described below — not raw aliases to a tar archive, but
  per-target `cc_import` / `cc_library` / `sh_binary` / filegroup
  rules referencing files extracted from the install_tree.
- The genrule still writes `BUILD.bazel.out` and exits 0.
- A sibling `failure.json` is still written so the orchestrator
  / audit can see that fallback fired (with the same per-call
  triage report Phase A emits).

### Placeholder shape (per-target rules from install destinations)

Aliasing every cmake target to the coarse `install_tree.tar`
doesn't work — Bazel can't link a tar archive into a
downstream `cc_binary`. Downstream consumers expect the same
ergonomics native render gives them: `deps = [":thelib"]`
should pull in linkable artifacts and headers.

The codemodel records `Target.Install.Destinations[].Path` per
target (relative to `Target.Install.Prefix.Path`) plus
`Target.NameOnDisk` (the artifact filename). Combined, that's
enough to reconstruct each target's path inside the
install_tree without parsing tar contents at action time. The
placeholder generator walks the codemodel exactly the way
`lower.lowerTarget` does today, but emits per-target stub
rules instead of full cc rules.

Sketch of the generated BUILD shape:

```python
# One-time tar extract genrule. Outputs are enumerated from
# Target.Install.Destinations + NameOnDisk, plus a header tree
# under include/.
genrule(
    name = "<elem>_install_tree_extract",
    srcs = ["//<elem-pkg>:install_tree.tar"],
    outs = [
        "tree/lib/libthelib.a",
        "tree/include/thelib.h",
        "tree/bin/thetool",
        # ...one entry per (target, install destination) plus
        # the headers the codemodel records as installed.
    ],
    cmd = "tar -C $(@D)/tree -xf $<",
)

# Per-target stub rules. Kind dispatch follows Target.Type:
cc_import(
    name = "thelib",                   # STATIC_LIBRARY
    static_library = "tree/lib/libthelib.a",
    hdrs = ["tree/include/thelib.h"],
    visibility = ["//visibility:public"],
)
cc_import(
    name = "shared_lib",               # SHARED_LIBRARY
    shared_library = "tree/lib/libshared_lib.so",
    hdrs = [...],
)
sh_binary(
    name = "thetool",                  # EXECUTABLE
    srcs = ["tree/bin/thetool"],
)
cc_library(
    name = "headers_only",             # INTERFACE_LIBRARY
    hdrs = [...],
    includes = ["tree/include"],
)
```

Targets without an `Install` block — utility, internal-only
libraries, the project's private build artefacts — are
**omitted** from the placeholder. Downstream consumers
referencing such labels get a Bazel "label not found" error
that's a clear signal: the target wasn't part of the install
contract; either expose it via `install()` upstream or stop
depending on it across the round-2 boundary.

The codemodel also records `compileGroups[].includes[]` for
each target, which `lower.lowerTarget` already walks for
header discovery. The placeholder generator reuses that walk
to populate `hdrs` on `cc_import` / `cc_library` rules from
the installed `include/` tree, preserving the public-header
contract.

### Why not filegroups everywhere?

A simpler shape — emit a `filegroup(srcs = ["tree/lib/libthelib.a"])`
per target — wouldn't satisfy `cc_binary`'s `deps` interface
(filegroups don't carry the `CcInfo` provider). `cc_import`
gets the linkable `CcInfo` automatically. We use `filegroup`
only for the few target types that don't fit any cc_* rule
(custom-command outputs, runtime data, etc.).

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
2. **convert-element `--unsupported-execute-process=fallback`
   flag.** When set, classifier refusals become a sentinel;
   `lower.ToIR` emits a placeholder `ir.Package`. Add a
   golden test for the placeholder shape. write-a doesn't yet
   pass the flag, so live behavior is unchanged.
3. **write-a `--cmake-round2-fallback` flag + per-element
   round-2 install genrule emission.** When the flag is on
   AND the element opts in (via a `.bst` field, or the
   default for projects with detected `execute_process`),
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
