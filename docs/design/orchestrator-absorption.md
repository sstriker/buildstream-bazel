# Folding `orchestrator/` into the write-a + Bazel path

## Context

The repo has two multi-element drivers:

- **`orchestrator/cmd/orchestrate`** — the original. One-pass:
  the orchestrator process *is* the scheduler. It walks the
  `.bst` graph, runs `convert-element-cmake` per element in
  topo order, owns a local/gRPC CAS + ActionCache layer, and
  can submit each per-element conversion as a REAPI Action so a
  remote Buildbarn cluster fans the work out.
- **`cmd/write-a` + Bazel** — the newer shape. Two-pass:
  write-a *renders* project A and project B; **Bazel**
  schedules the cross-element work by running the per-element
  converters as genrules.

Only the write-a path can express the trace-driven 3 → 2′ loop
(see `docs/three-pass-flow.md`) that non-cmake kinds depend on,
so it is the one with a future. The orchestrator cannot absorb
the two-pass model; the fold only runs one way.

This doc is the capability-by-capability map — what to
**delete**, what to **re-home**, what to **keep** — plus a
bottom-up PR sequence. It exists because `ROADMAP.md`'s
"Fold `orchestrator/` into the write-a + Bazel path" bullet
needs a concrete plan behind it.

## Why the orchestrator's headline feature is mostly redundant

The orchestrator's distinctive asset is **REAPI remote-execution
fan-out**: it submits each per-element conversion as a REAPI
`Action` and lets a Buildbarn cluster execute it.

Once Bazel is the scheduler, that is largely redundant. **Bazel
speaks REAPI natively** — point it at a Buildbarn cluster with
`--remote_executor` and the per-element converter genrules fan
out across the executor pool with no bespoke submission code.
The orchestrator's `internal/orchestrator` REAPI path is
re-implementing, in ~1700 lines of Go, a slice of what Bazel
already does.

But "largely redundant" is a claim that has to be **proven by a
real CI gate**, not assumed. Before the orchestrator's REAPI
path is deleted, a CI job must show the write-a + Bazel path
running per-element conversions on a real Buildbarn cluster via
Bazel-native `--remote_executor`, *and* doing so build-without-
the-bytes (`--remote_download_minimal` / the bb_clientd mount)
so intermediate artifacts never round-trip through local disk.
That gate exercises the production-intended setup — real
`bazel`, the `deploy/buildbarn/` stack — **not** a Go test
harness or any code kept alive only to satisfy CI. It is step 6
of the sequence below, and step 7 (the delete) blocks on it
being green and stable.

One genuinely open sub-question rides along: the orchestrator's
`--platforms-json` carries `reapi_properties` per platform for
per-platform executor routing; write-a's `--platforms-json`
deliberately ignores that field today. That routing has to land
on the Bazel-platform / `exec_properties` side, and the step 6
gate is where it gets verified.

## Isolation check (done)

Nothing outside `orchestrator/` imports any `orchestrator/`
package — verified with
`grep -rn "buildstream-bazel/orchestrator" cmd/ internal/ converter/`.
`cmd/write-a` imports zero orchestrator code. The fold therefore
never has to thread a compatibility shim through the write-a
path; every component below is independently movable or
deletable.

Shared substrate already lives at the repo-root `internal/`
(`cas`, `reapi`, `manifest`, `fidelity`, `shadow`, `readpaths`,
`tracenorm`, `synthprefix`) and is **not** part of
`orchestrator/` — it stays put regardless.

## Capability map

### Delete — redundant once Bazel is the scheduler

| Component | LOC | Why it goes |
|---|---|---|
| `orchestrator/cmd/orchestrate` | ~420 | The scheduler process itself. Bazel replaces it. |
| `orchestrator/internal/orchestrator` | ~1700 | Concurrency loop + AC/CAS layer + REAPI submit path. Bazel's native scheduling + remote cache + RBE replace all of it. |

### Re-home — genuine value, not a scheduler concern

| Component | LOC | Destination | Notes |
|---|---|---|---|
| `orchestrator/internal/element` | ~500 | merge into the write-a parser | The rigorous dep-graph code (cycle detection, topo sort, junction handling). write-a's `loadGraph` / `discoverBstGraph` already do cycle detection + topo sort; the gap to close is junction handling. End state: **one** `.bst` parser. See "Parser consolidation" below. |
| `orchestrator/cmd/orchestrate-diff` | ~120 | `cmd/render-diff` (or keep the name) | Run-vs-run regression diff. Standalone CLI; just needs to read the write-a path's run outputs instead of `orchestrate`'s. |
| `orchestrator/cmd/orchestrate-history` | ~140 | `cmd/render-history` | Fingerprint-history queries. Same story as diff. |
| `orchestrator/internal/regression` | ~920 | `internal/regression` | Backs diff + history. The work is repointing `LoadRun` at whatever the write-a path defines as a "run" (see Open questions). |
| `orchestrator/cmd/orchestrate-bst-translate` | ~350 | `cmd/bst-translate` | `.bst` → `kind:remote-asset` rewriting. No scheduler dependency — a near-pure move. |
| `orchestrator/internal/bsttranslate` | ~160 | `internal/bsttranslate` | Backs bst-translate. Pure move. |
| `orchestrator/internal/sourcecheckout` | ~320 | `cmd/source-checkout` or a write-a pre-step | Resolves `kind:local` / `kind:git` / `kind:remote-asset` to local trees. write-a today only *consumes* a pre-fetched `--source-cache`; this is the fetcher that populates it. Genuinely needed. |
| `orchestrator/internal/exports` | ~240 | `internal/exports` | Parses `<Pkg>Targets.cmake` from the cmake-config bundle into a manifest. Re-home **if** the write-a path grows cmake-config-bundle consumption; otherwise it can wait. |
| `orchestrator/internal/allowlistreg` | ~140 | evaluate vs `internal/readpaths` | Per-element cmake-read allowlist registry. write-a already has `internal/readpaths` + the narrowing audit; check for overlap before re-homing — this may partly *merge* rather than move. |

### Keep — already shared, untouched

The repo-root `internal/` packages. `synthprefix` in particular
is already at `internal/synthprefix` (not under `orchestrator/`),
so whichever code needs synthetic `CMAKE_PREFIX_PATH` stub trees
just keeps importing it.

## Parser consolidation

Step 1 (shipped) pointed `tools/bst` at write-a's parser via
`--bst-root`, taking the repo from three `.bst` graph walkers to
two. The remaining two:

- `cmd/write-a`'s `loadGraph` / `discoverBstGraph` — leaf-rooted
  or explicit-set, rich on YAML semantics (`(?):` conditional
  folding, `(@):` includes, per-kind source resolution).
- `orchestrator/internal/element` — project-directory-rooted
  (`ReadProject`), rigorous on the graph itself (cycle
  detection, topo sort, junction errors).

End state: **one** parser. Because every `orchestrator/internal/element`
consumer is on the delete-or-re-home list, the natural endpoint
is write-a's parser as the sole walker, with `element`'s
junction-handling rigor ported across before `element` is
deleted. The re-homed analysis tooling (diff / history) then
depends on the write-a parser too. If a project-directory-rooted
entry point is still wanted (orchestrate's `ReadProject` shape),
add it as a second entry point on the write-a parser rather than
keeping a second implementation alive.

## PR sequence (bottom-up, each shippable)

1. **`tools/bst` → `--bst-root`** *(shipped)*. write-a does
   leaf-rooted discovery through its own parser; the shell awk
   graph walker is gone. 3 → 2 walkers. Also fixed the
   `Makefile` so binary targets actually rebuild on source
   change (they had no prerequisites), which `tools/bst`'s
   `ensure_binaries` was silently relying on.
2. **Parser consolidation** *(in progress)*. `internal/element`'s
   one piece of rigor the write-a parser lacked — explicit
   rejection of junction-crossing deps — is now ported:
   `loadGraph` and `discoverBstGraph` both fail with a clear
   "junctions not yet supported" diagnostic instead of letting a
   junctioned filename fall through to a confusing
   "not in the graph" / missing-sibling error. A project-rooted
   (walk-all-`.bst`-under-a-directory) entry point is *not* added
   speculatively — `internal/element`'s `ReadProject` shape is
   only needed by `orchestrate` (deleted in step 7) and
   `orchestrate-bst-translate` (re-homed in step 4), so that
   entry point lands with step 4 if it's still wanted then.
   `internal/element` is not deleted yet — its consumers are
   still live — but the write-a parser now has feature parity, so
   it is the sole walker the live write-a path needs.
   Also done in this pass: `orchestrator/internal/element` moved
   to `internal/element` (it's a leaf, and every re-homed package
   depends on it), and the dead `orchestrator/internal/translate`
   (zero importers) was deleted.
3. **Re-home the analysis tooling** *(done — move only)*.
   `internal/regression` → `internal/regression`,
   `orchestrate-diff` / `orchestrate-history` → `cmd/`. Binary
   names kept for now. `regression`'s production code imports
   only `internal/element`; the `internal/orchestrator` coupling
   is confined to its `e2e`-tagged test, flagged in-file.
   **Still open:** repointing `LoadRun` at the write-a path's run
   outputs (the "what is a run" question below) — that functional
   rework, and the binary rename that naturally rides with it,
   is follow-up, not part of the move.
4. **Re-home `bst-translate`** *(done)*. `orchestrate-bst-translate`
   → `cmd/bst-translate` (binary renamed), `internal/bsttranslate`
   → `internal/`. Near-pure move; only `internal/element` in its
   import set.
5. **Re-home the source + manifest helpers** *(done — move only)*.
   `sourcecheckout`, `exports`, `allowlistreg` → `internal/`. Each
   imported only already-shared `internal/` packages, so these
   were pure moves. **Still open:** wiring `sourcecheckout` as the
   `--source-cache` populator, and the `allowlistreg` vs
   `internal/readpaths` overlap analysis — follow-up.
6. **Stand up the write-a + Bazel + Buildbarn remote-execution
   gate** *(v1 shipped; pass B is follow-up)*. A CI job that
   drives `write-a` → `bazel build` against the real
   `deploy/buildbarn/` stack with Bazel-native `--remote_executor`,
   verifying (a) per-element converter genrules actually execute
   on Buildbarn workers — the RE semantics the orchestrator's
   Action-submission path covers today via `e2e-buildbarn` /
   `e2e-buildbarn-execute` — and (b) the build stays
   **build-without-the-bytes** (`--remote_download_minimal`, or
   the bb_clientd mount), with intermediate outputs never
   materialized locally. The job must exercise the
   production-intended setup end to end; it is explicitly *not*
   allowed to lean on a bespoke Go test harness or any code path
   kept alive only for CI.

   **Shipped:** `tools/e2e-meta-buildbarn-re.sh` (`make
   e2e-meta-buildbarn-re`, wired into the `buildbarn-e2e` CI job).
   It renders project A from the kind:cmake hello-world fixture,
   injects an operator-style `.bazelrc` + `platform()` pointing at
   bb-storage `:8980` / bb-scheduler `:8983`, and asserts the
   `convert-element-cmake` genrule executes remotely
   (`--strategy=Genrule=remote` makes a local fallback a hard
   failure) with its output never materialized locally
   (`--remote_download_minimal`). That covers **pass A** — the
   converter genrule.

   **Follow-up (pass B):** project B's `cc_*` compiles on the
   remote worker additionally need a cc-toolchain-for-RBE
   configuration (the rendered project uses the autodetected
   *local* toolchain today, which won't resolve against a remote
   exec platform). Pass A alone already exercises the core thesis
   — a converter genrule running remotely, bytes staying remote.

   This step is independent of steps 3–5 and can be done in
   parallel, but it must be green and stable before step 7. The
   current `e2e-buildbarn` / `e2e-buildbarn-execute` jobs are
   *replaced* by this gate, not merely dropped — the coverage has
   to move, not vanish.
7. **Delete the scheduler.** Remove `orchestrator/cmd/orchestrate`,
   `orchestrator/internal/orchestrator`, and (now that its
   consumers have moved) `orchestrator/internal/element`. The
   `e2e-buildbarn` / `e2e-buildbarn-execute` jobs retire here
   (their coverage moved to step 6's gate); the `e2e-orchestrate`
   / `e2e-orchestrate-scale` jobs are dropped. Update
   `docs/architecture.md` + `README.md`. Move the `ROADMAP.md`
   bullet to Done.

After step 7 the `orchestrator/` tree is empty and removed.

## Open questions

- **Per-platform executor routing.** Step 6's gate proves
  Bazel-native RBE covers per-element conversion on Buildbarn,
  but the orchestrator's `reapi_properties`-driven per-platform
  routing is the one piece with no write-a equivalent yet — it
  has to land on the Bazel-platform / `exec_properties` side.
  The open question is the exact mapping from `--platforms-json`'s
  `reapi_properties` onto `exec_properties` on the converter
  genrules; resolved as part of step 6.
- **What is a "run" for the regression diff?** `internal/regression`'s
  `LoadRun` reads `orchestrate`'s `converted.json` /
  `failures.json` / `determinism.json`. The write-a path's
  equivalent "run output" is the rendered project A/B plus the
  Bazel build result. Step 3 has to define that schema before
  diff / history can be repointed.
- **`allowlistreg` vs `readpaths`.** Both track per-element
  cmake-read paths. Determine in step 5 whether `allowlistreg`
  re-homes whole or partly collapses into `internal/readpaths`.
