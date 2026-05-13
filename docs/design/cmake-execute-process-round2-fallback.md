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
`convert-element-cmake` against cmake's File API reply. There's no
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
per-element fallback that activates when convert-element-cmake exits
with `unsupported-execute-process`**. Option (1) would force
every `kind:cmake` element through the round-2 path
unconditionally (the pipeline-kinds and autotools opt-ins are
both build-wide, not per-element), which sacrifices native
render's fine-grained `cc_library` / `cc_binary` outputs for
projects that don't need the fallback.

## Architecture: A converter + B install + round-2 rendezvous

`kind:cmake` round-2 fallback uses the same architectural
split that `kind:autotools` round-2 already established:

- **Project A — converter genrule.** Runs `cmake configure` +
  File API + `lower`. Decides per-element at action time
  whether to emit native cc rules or the placeholder shape
  in `BUILD.bazel.out`. `convert-element-cmake`'s executor
  toolchain stays cmake-only — no ninja, no install. The
  per-element decision is "what shape of `BUILD.bazel.out`
  to write," not "what build to run."
- **Project B — install genrule.** `build-tracer` wraps
  `cmake -B build` + `cmake --build build` + `cmake --install
  build`. Outputs `install_tree.tar` (split per cmake's
  `install_manifest.txt` is a future optimisation; v1 emits
  a single tar). Inline `trace-publish` lands the AC entry
  keyed by `SyntheticActionDigest(srckey)`.
- **Round-2 rendezvous.** A's converter genrule consumes
  `@trace_<elem>//:trace` via load-time `_trace_repo` lookup
  against the REAPI ActionCache. Same mechanism + same
  helpers as kind:autotools round-2 (see
  `docs/design/autotools-round2-rendezvous.md`).

write-a's responsibility expands by exactly the kind:autotools
amount: emit both A's converter genrule and B's install
genrule for kind:cmake elements when round-2 fallback is
enabled. The "is this element a fallback element" decision
isn't write-a's — write-a uniformly emits both genrules for
every kind:cmake element under round-2 mode. Bazel evaluates
B's install genrule lazily; it's only built when A's
`BUILD.bazel.out` references its outputs. Native-render
elements emit cc rules that don't reference `install_tree.tar`,
so B's install genrule sits unbuilt; fallback elements emit
the placeholder shape that does reference it, so B's install
genrule runs.

### Convergence: the trace turns refusal into refinement

The round-2 rendezvous gives the architecture an interesting
property: refused elements aren't permanently coarse. The
first build of a given srckey hits the miss path (trace
empty → placeholder shape with `cc_import` / `sh_binary`
stubs); after pass-3 publishes the trace + AC entry,
subsequent builds of the same srckey *anywhere* (CI, dev
laptop, fresh executor) see the trace at A's load time and
the converter can refine — possibly all the way to native
cc rules for the previously-refused element, by reading the
trace to learn what the unliftable `execute_process` actually
did at the filesystem level.

v1 of the converter doesn't do that refinement; it always
emits the placeholder shape when classification refuses,
regardless of whether a trace is available. The converger
direction is the natural follow-on once placeholder-shape
projects are in real use — it's queued as a Later bullet
in `ROADMAP.md`.

### Why not run the build inside convert-element-cmake

Bolting `cmake --build` + `cmake --install` onto
convert-element-cmake would keep the per-element decision in one
place but at three real costs:

1. **Toolchain expansion.** Executors that run convert-element-cmake
   gain a ninja dependency and a DESTDIR-shaped sandbox.
   Currently the convert-element-cmake action is a pure
   static-analysis tool; conflating it with a build action
   muddies the role.
2. **No round-2 rendezvous.** The kind-agnostic
   `_trace_repo` + `trace-publish` plumbing assumes B's
   install genrule emits the trace; folding into A
   means re-implementing the rendezvous A-side, with no
   share with kind:autotools/make.
3. **No convergence path.** With B emitting the trace, the
   round-2 mechanism above — refused elements becoming
   fine-grained over time — falls out for free. With A
   doing the build, the trace would be A's own action's
   fingerprint, not the upstream build's, defeating the
   convergence direction.

So the split lives where kind:autotools put it: A is
analysis, B is build. write-a renders both.

## The shape

```
write-a --cmake-round2-fallback  →
  project A: per-element converter genrule
             outs: [BUILD.bazel.out, ...]
             cmd: convert-element-cmake --reply-dir=... \
                  --unsupported-execute-process-fallback=true \
                  --out-build=BUILD.bazel.out
             load-time: @trace_<elem>//:trace via _trace_repo
  project B: per-element install genrule
             outs: [install_tree.tar, trace.log, ...]
             cmd: build-tracer wraps:
                  cmake -B build -G Ninja -S src \
                  cmake --build build --parallel 1
                  cmake --install build --prefix=$DESTDIR
                  tar -C $DESTDIR -cf install_tree.tar .
                  trace-publish (lands the AC entry)

bazel build A//<elem>:<elem>_build
   pass-2 converter genrule:
     load-time → _trace_repo lookup against AC
                 → AC hit  ⇒ symlink trace dir into @trace_<elem>//
                 → AC miss ⇒ empty fileset
     action time → cmake configure + File API reply + trace.jsonl
                   convert-element-cmake runs lower.ToIR(...)
                     → native success ⇒ emit fine cc rules
                     → unsupported-execute-process refusal AND
                       fallback flag set ⇒ emit placeholder:
                         * extract genrule untarring
                           //<elem-pkg>:install_tree.tar
                         * per-target stubs:
                             STATIC/OBJECT → cc_import + static_library
                             SHARED/MODULE → cc_import + shared_library
                             EXECUTABLE    → sh_binary + srcs
                             INTERFACE     → cc_library hdrs-only

bazel build B//<elem>:<elem>_install
   pass-3 install genrule (only built when A's BUILD.bazel.out
   references install_tree.tar — i.e. fallback path was taken):
     build-tracer wraps:
       cmake -B build [...]
       cmake --build build
       cmake --install build --prefix=$INSTALL_ROOT
     synthesize empty make-db.txt (cmake has no make-db; the
       trace-publish wire contract requires the slot)
     trace-publish lands the AC entry
```

Bazel evaluates B's install genrule lazily. Native-render
elements emit cc rules that don't reference
`install_tree.tar`; B's install genrule sits unbuilt for them.
Fallback elements emit the placeholder shape that *does*
reference `install_tree.tar`; B's install genrule runs.

## Convert-element's failure → placeholder transition

Today, `convert-element-cmake` exits 1 on `unsupported-execute-process`
and the genrule action fails. For the fallback, the genrule
action must succeed even when classification refuses, and emit
a placeholder BUILD whose per-target stubs delegate to
Project B's `install_tree.tar`.

Implementation: a new `--unsupported-execute-process-fallback`
flag on `convert-element-cmake`. When set:

- Classifier's refusal path no longer returns Tier-1; instead
  `recoverExecuteProcess` returns a structured refusal slice
  the lowering reads.
- `lower.ToIR` calls `emitFallbackPlaceholder` to produce an
  `ir.Package` of per-target stubs (shape described below).
  Convert-element's executor toolchain is unchanged — still
  cmake-only, no ninja, no install. The actual build work
  lives in Project B's install genrule (Step 3, write-a side).

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
# is "install_tree.tar" — a label that resolves once A's
# BUILD.bazel.out is symlinked into Project B's package and
# co-locates with B's install genrule (which produces
# install_tree.tar as one of its outs).
genrule(
    name = "_install_tree_extract",
    srcs = ["install_tree.tar"],
    outs = [
        "install_tree/lib/libthelib.a",
        "install_tree/lib/libshared.so.1",
        "install_tree/bin/thetool",
        # ...one entry per (target, install destination).
    ],
    cmd = "mkdir -p \"$(RULEDIR)/install_tree\" && tar -C \"$(RULEDIR)/install_tree\" -xf \"$(location install_tree.tar)\"",
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
2. **convert-element-cmake `--unsupported-execute-process-fallback`
   flag.** When set, classifier refusals stop exiting Tier-1;
   `lower.ToIR` returns a placeholder `ir.Package` (initially
   per-target empty stubs in PR #97; upgraded to full
   per-target `cc_import` / `sh_binary` / extract-genrule
   shape in Step 2.5 — PR #98). The flag is off by default;
   existing flows unchanged.
3. **write-a integration: emit Project B install genrule +
   wire the round-2 rendezvous.** write-a's kind:cmake render
   gains a round-2 mode that:
   - threads `--unsupported-execute-process-fallback=true`
     into A's converter genrule cmd;
   - adds the `@trace_<elem>//:trace` load-time lookup to
     A's converter genrule (kind-agnostic helpers in
     `handler_pipeline_round2.go::renderTraceDrivenRound2A`
     are reusable as-is);
   - emits B's install genrule wrapping cmake configure +
     ninja + install under `build-tracer` (using the
     `wrapCmakePipelineCmds()` helper from Step 1) plus
     inline `trace-publish`.
   Render gate `scripts/meta-cmake-round2-fallback.sh` lands
   here. Reuses `cmd/build-tracer`, `cmd/trace-publish`,
   `cmd/trace-lookup`, and the synthetic-key digest from
   `internal/tracenorm/synthkey.go` — all kind-agnostic.
4. **Roadmap promotion + docs.** Move the ROADMAP `Later`
   bullet to `Now`/`Done` as the steps land. Update
   `docs/research/cmake_analysis.md` §9 to reflect the
   shipped fallback path.

### Future: trace-driven convergence (research)

A natural Step 5 (queued in ROADMAP `Later`): with B's trace
available via `@trace_<elem>//:trace`, the converter can read
it to learn what the unliftable `execute_process` actually
spawned + wrote at the filesystem level — and potentially
emit *fine-grained* `cc_library` / `cc_binary` rules for
elements that originally refused. The first build of a given
srckey hits the placeholder shape; subsequent builds *anywhere*
(after pass-3 publishes) see the trace and refine. This makes
fallback a transient state, not a permanent regression. v1
doesn't ship this; it's tracked as a research direction.

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
