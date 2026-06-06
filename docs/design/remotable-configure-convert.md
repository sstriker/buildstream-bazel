# Remotable, cacheable configure + convert (the two-species split)

> **Lifespan.** This is a *design* doc for unbuilt work, not permanent
> architecture. **Delete it once the split has landed** — the code plus
> `ROADMAP.md` then become the record, per the repo's doc conventions
> (`CLAUDE.md`: architecture docs describe how systems work today; they
> don't restate plans). Captured here so the reasoning isn't re-derived.

## Problem

`convert-element-cmake` today execs cmake **in-process** (`cmakerun.Configure`)
and then loads the File API reply + lowers + emits BUILD, all in one process on
one node. That welds two incompatible requirements into a single action:

- **cmake configure** must run on the *target* platform P (its `try_compile` /
  `try_run` / `check_*` probes compile and run code; toolchain detection and
  `find_package` resolve against P's filesystem). An element may target a
  *subset* of platforms.
- **the converter** is a Linux/Go binary. It is **not** built for every
  platform — but the platforms where cmake is available are often platforms
  where the converter is not.

We want **every action (configure, convert) to be independently remotable and
cacheable**: a configure that runs natively on P (no Go), and a convert that
runs on Linux (no cmake) consuming configure's output.

## The two species

| species | needs | execs on | inputs | outputs |
|---|---|---|---|---|
| **`configure(element, P)`** | cmake + a P toolchain; **no Go** | platform **P** (native) | narrowed sources + staged File API query/hooks | reply bundle |
| **`convert(element)`** | Go; **no cmake** | **Linux** | the reply bundles of every P the element targets | BUILD.bazel + exports + bundle |

A third role, the **planner** (`cmd/write-a`, Linux, graph-construction time),
emits the action graph. The per-platform fan-out + fold already exists
(`--out-ir-json` → `fold-element`, `internal/configfold`); native-P configure
just retargets each per-platform action's `exec_compatible_with` to P's
constraints + the worker's advertised `exec_properties` (the `platform()`
mechanism `tools/e2e-meta-buildbarn-re.sh` already proves). Running configure
**natively** on P (rather than cross-compiling on Linux) also removes the
`try_run` cross-compile fidelity hole documented in
`docs/research/cmake_analysis.md` §7.

## Why the split is mostly assembly, not invention

The decoupling seam already ships:

- **`--reply-dir`** makes the converter consume a pre-existing File API reply
  with **zero cmake** — the `converter/testdata/fileapi/` fixtures are replies
  recorded on another machine and replayed offline. Promote that from "offline
  fixture path" to *the normal distributed path*: `configure(P)` **produces**
  the reply bundle; `convert` consumes it via `--reply-dir`.
- **The File API query is language-agnostic** — five touch-files
  (`codemodel-v2`, `toolchains-v1`, `cmakeFiles-v1`, `cache-v2`,
  `configureLog-v1`; `cmakerun/run.go`). The dump-vars / probe `.cmake` hooks
  are static text. So a `configure(P)` action is just `cmake <argv>` with those
  staged as inputs and the reply dir + traces declared as outputs. No Go on P.
- **argv + query + hook construction is a pure function of the options**, today
  in `cmakerun`. The planner calls the *same* package to render the action
  command; it is **not** forked out of the converter (see the standalone
  invariant below).
- **Reply hermeticity off-box** (absolute build/source/compiler paths, scratch
  dir names) is already handled by the recorded-reply path's host/recorded dir
  remapping and the source-tree filtering in the shadow extractors.

## The two-pass genex literal probe under the split

The converter resolves arbitrary `$<…>` literals via a warm second cmake
configure with a `file(GENERATE)` probe hook whose content **is** the
discovered-unresolved-literal list. In-process this reuses a warm build dir and
is skipped when nothing is unresolved. Hermetic remote actions don't share a
warm dir, and the probe content depends on pass-1's trace — so a naive
"conditionally emit pass 2" is a dynamic action graph, which Bazel doesn't do.

**Resolution — a static graph with a data-dependent wrapper:**

```
configure(P)   narrowed sources                    → reply1 + trace1 (+ structural probe)
analyze        trace1                   [Go,Linux]  → probe.cmake     (0 bytes if no literals)
litprobe(P)    narrowed sources + probe.cmake       → litgenex/       (probe results; empty if probe empty)
convert        reply1 + litgenex        [Go,Linux]  → BUILD
```

- **Fully static graph, pure artifact edges** — `analyze`'s `probe.cmake` rides
  a normal edge into `litprobe`. No dynamic deps, no driver.
- **`litprobe` short-circuits via its *command*, not the action cache.** Two
  distinct Bazel actions can't share an output path (action conflict), and
  `output_paths` are part of the REAPI action digest — so `litprobe` will never
  AC-collapse onto `configure` even with an empty probe. Instead `litprobe`'s
  command branches on the 0-byte probe:

  ```sh
  if [ -s probe.cmake ]; then
      cmake -S … -B … -DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=…/probe.cmake …   # real generate pass
      cp -r build/cmake-to-bazel.litgenex/. litgenex/
  else
      : > litgenex/.empty   # no literals → emit empty result, NO cmake
  fi
  ```

  Pure shell on the worker (no Go). When empty, `litprobe` does no cmake work.

### Cost model for the empty (common) case

Giving **pass 1 a 0-byte probe too** makes `configure` and `litprobe` have
**byte-identical input roots** in the empty case (same narrowed sources, same
empty-blob probe); only `output_paths` differ. Consequences:

- **input upload ≈ 0** — sources are already in CAS from `configure`; the probe
  is the universal empty-SHA256 blob, always resident.
- **worker materialization ≈ warm** — `litprobe`'s input tree is the one the
  worker just hardlinked for `configure`.
- **source narrowing keeps *both* passes small-tree actions**, and — the bigger
  win — makes the AC **precise**: configure / non-empty-litprobe re-run only
  when a file cmake actually *reads* changes, not on unrelated churn.
- **execution** is `test -s` + an empty write.

The only residual the above does **not** remove is the **per-element scheduling
round-trip** (enqueue → dispatch → AC write) — fixed overhead, independent of
bytes, paid on *cold* builds only (an AC hit on every rebuild). If that tax is
unwanted at fleet scale, make `litprobe` **opt-in** and let the rare
arbitrary-adjacent-genex literal fall back to the existing `-unresolved` bake
(accepted Phase-3 residue) — the fidelity ceiling is unchanged.

### Narrowing the configure inputs (bootstrap nuance)

Two granularities:
- **Element-level (static):** the action's declared inputs = the element's
  source subtree, known from the `.bst`/package layout with no trace. The floor,
  available on the first build.
- **Read-path-level (`read_paths.json`, finer):** narrowing to exactly the files
  cmake reads needs the trace, which needs a configure. An action's *declared*
  inputs can't shrink from its own output within one build, so this is a
  **rendezvous-fed refinement** (publish `read_paths`, the next render declares
  the tighter set — the same rendezvous channel, not the convergence loop). The
  `narrowing-audit` gate guards that the tightened set is sufficient.

Over-declaring in the meantime costs only cache precision, never correctness.

## Invariant: the standalone path must keep working

A developer with cmake and the converter binary — **no Bazel, no Buildbarn, no
driver** — must keep getting a complete, full-fidelity BUILD from
`convert-element-cmake --source-root`. The distributed design is a **different
composition of the same steps**, not a reimplementation. Three disciplines
enforce this:

1. **`cmakerun.Configure` stays a callable in-process API; `--source-root` stays
   self-contained.** The Bazel-action wrapping is an *additional* entry point,
   never a replacement. Standalone keeps the in-process configure + warm two-pass
   (the `litprobe` wrapper is just the distributed realization of the in-process
   "skip pass 2 when nothing's unresolved").
2. **argv / query / hook construction lives in one shared package**, called by
   both the in-process converter and the planner — no fork that can drift.
3. **The serialized reply bundle is a complete interface**: everything `convert`
   consumes from `configure` (reply JSON, both traces, vars dump, structural +
   literal probe outputs) is in the bundle, so `--reply-dir` is a *full*
   substitute for in-process configure. Anything a future change makes `convert`
   consume from configure must go into the bundle, never in-process-only state.

**Regression guard:** the `meta-cmake-*.sh` render gates already run
`convert-element-cmake --source-root` with no Bazel in the loop — they *are* the
standalone contract. Add a gate asserting `--reply-dir` produces byte-identical
output to the in-process run for the same element (proving the bundle is a
complete interface).

## Caching & determinism

`configure` cache key = {narrowed sources, cache vars, staged hooks,
cmake/toolchain identity via `exec_properties`, platform}. cmake configure has
non-hermetic bits (scratch dir names, timestamps), but AC correctness doesn't
require deterministic *outputs* — the AC maps input-digest → output-digest, so
the first run's reply is cached and reused on input match. The deterministic-
given-a-reply layer is `convert`, which already filters scratch noise. Pinning
cmake/ninja into `exec_properties` (already done in
`deploy/buildbarn/config/worker.jsonnet`) is what makes *"same source + same
toolchain + same converter version → same AC, shared across nodes"* hold.

## Where rendezvous fits

The rendezvous AC-lookup (`rules/traces.bzl` `_trace_repo`, loading-time
`GetActionResult`) stays the tool for the **cross-element / cross-project**
handoff (find_package deps, install trees) and for feeding `read_paths` to the
next render. It is **not** used for this element's configure/litprobe — those are
ordinary remotable actions wired by artifact edges. Pulling configure into a
repo-rule rendezvous would run cmake at loading time, which **doesn't execute on
RBE** — the disqualifier the install repo-rule was already rejected for.

## Acceptance

- `configure(element, P)` runs native cmake on a P worker (no Go), emits a reply
  bundle; `convert(element)` runs on Linux (no cmake) consuming it. Both are
  REAPI-cacheable on their declared inputs.
- The `configure → analyze → litprobe → convert` graph is static; the empty
  `litprobe` does no cmake and shares `configure`'s input root.
- `convert-element-cmake --source-root` remains a complete, infrastructure-free
  path at full fidelity, and `--reply-dir` is byte-identical to it (new gate).
- Per-platform fan-out + fold is unchanged; native-P configure closes the
  `try_run` cross-compile gap.
