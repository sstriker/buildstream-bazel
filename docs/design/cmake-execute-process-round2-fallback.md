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

`kind:autotools / make / etc.` joined round-2 via a one-line
opt-in: each one is a `pipelineHandler` variant
(`cmd/write-a/handler_pipeline.go:51`), and round-2 dispatch
hangs off the `traceDrivenSrckeyPatterns` field
(`handler_pipeline.go:85`). When the field is non-nil and
runtime config enables round-2,
`pipelineHandler.shouldUseRound2()` returns true and `RenderA` /
`RenderB` route through the kind-agnostic helpers in
`handler_pipeline_round2.go`.

`kind:cmake` is **not** a `pipelineHandler`. `cmakeHandler` is
its own `struct{}` (`handler_cmake.go:39`) with bespoke
`RenderA` / `RenderB` that don't go through the pipeline
dispatcher. The round-1 / round-2 split there isn't a
configure-then-make-then-install shape; it's a single per-element
genrule that runs `convert-element` against cmake's File API
reply. There's no `traceDrivenSrckeyPatterns` field to set; no
`shouldUseRound2` branch to flip.

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
unconditionally (the opt-in is build-wide, not per-element),
which sacrifices native render's fine-grained `cc_library` /
`cc_binary` outputs for projects that don't need the fallback.

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
emit a placeholder pointing at the round-2 install_tree.tar.

Implementation: a new `--unsupported-execute-process=fallback`
flag on `convert-element`. When set:

- Classifier's refusal path no longer returns Tier-1; instead
  `recoverExecuteProcess` returns a sentinel value the lowering
  reads.
- `lower.ToIR` returns a *placeholder* `ir.Package` containing
  alias rules for every cmake target the codemodel records,
  each pointing at `:<elem>_install_tree.tar` (or the unpacked
  contents thereof, depending on how the round-2 install
  genrule lays out artifacts).
- The genrule still writes `BUILD.bazel.out` and exits 0.
- A sibling `failure.json` is still written so the orchestrator
  / audit can see that fallback fired (with the same per-call
  triage report Phase A emits).

## The kind-specific bits

What's new for `kind:cmake` Phase B (everything else reuses the
kind-agnostic infra in `handler_pipeline_round2.go`):

- **`cmakeSrckeyPatterns()`** — the file-glob set that gates
  what counts as content-included for srckey computation.
  Proposed contents:
  - `CMakeLists.txt`, `**/CMakeLists.txt`, `cmake/*.cmake`,
    `cmake/*.cmake.in`, `**/*.cmake`, `**/*.cmake.in`
    (cmake-driven build commands).
  - `**/*.h` family + `**/*.hpp` + `**/*.hh` (header changes
    can shift include resolution, which surfaces in the
    trace).
  - Compile sources (`*.c` / `*.cc` / etc.) **path-only** —
    the trace records compile commands, not source bytes;
    edits to `.c` files don't change the trace's structure.
- **`wrapCmakePipelineCmds()`** — mirrors
  `wrapAutotoolsPipelineCmds()`
  (`handler_autotools_native.go:386`). Wraps cmake's
  `configure / build / install` sequence under build-tracer
  with `--normalize-prefix` for byte-stable traces.
  - Configure: `cmake -B "$$BUILD_DIR" -G Ninja -S "$$SRC_DIR" -DCMAKE_INSTALL_PREFIX="$$DESTDIR" [...]`
  - Build: `cmake --build "$$BUILD_DIR" --parallel 1`
  - Install: `cmake --install "$$BUILD_DIR" --prefix "$$DESTDIR"`
  The `--parallel 1` matches `make -j1` in autotools round-2
  for trace stability (no interleaved subprocess output).
- **A render gate** at `scripts/meta-cmake-round2-fallback.sh`,
  modeled on `meta-autotools-round2.sh` but asserting the
  cmake-specific shape (placeholder BUILD.bazel.out content,
  cmake-flavoured install genrule cmd).
- **A live-AC gate hookup** — the existing kind-agnostic
  `tools/e2e-meta-autotools-round2-live.sh` should pick up
  `kind:cmake` automatically once the render half is in place,
  per the rendezvous doc's Generality section.

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
   `--unsupported-execute-process=fallback` into the
   converter genrule's cmd. Render gate
   `meta-cmake-round2-fallback.sh` lands here.
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
  The render-half gate skips its bazel-build half cleanly
  when bazel ≥7 isn't on `$PATH`; same convention here.

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
| `tools/e2e-meta-autotools-round2-live.sh` | live-AC gate; covers kind:cmake automatically once Step 3 lands |
