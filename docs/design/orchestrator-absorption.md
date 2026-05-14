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

The thing to verify before deleting (see Open questions): that
Bazel-native RBE covers the orchestrator's **platform routing**
— the orchestrator's `--platforms-json` carries
`reapi_properties` per platform; write-a's `--platforms-json`
deliberately ignores that field today. Per-platform executor
routing has to land on the Bazel-platform / `exec_properties`
side before the orchestrator's routing can be dropped.

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
2. **Parser consolidation.** Port `internal/element`'s junction
   handling into the write-a parser; add a project-rooted entry
   point if needed. Don't delete `element` yet — its consumers
   are still live. 2 → 1 effective walkers.
3. **Re-home the analysis tooling.** Move `orchestrate-diff` /
   `orchestrate-history` to `cmd/`, `internal/regression` to
   `internal/`. Repoint `LoadRun` at the write-a path's run
   outputs. Keep CI green by keeping the old binaries building
   until the new ones are wired.
4. **Re-home `bst-translate`.** `orchestrate-bst-translate` →
   `cmd/bst-translate`, `internal/bsttranslate` → `internal/`.
   Near-pure move.
5. **Re-home the source + manifest helpers.** `sourcecheckout`
   as the `--source-cache` populator; `exports` and
   `allowlistreg` evaluated against the write-a path's existing
   `readpaths` / narrowing audit — re-home or merge as the
   overlap analysis dictates.
6. **Delete the scheduler.** Remove `orchestrator/cmd/orchestrate`,
   `orchestrator/internal/orchestrator`, and (now that its
   consumers have moved) `orchestrator/internal/element`. Drop
   or replace the `e2e-orchestrate` / `e2e-orchestrate-scale`
   CI jobs. Update `docs/architecture.md` + `README.md`. Move
   the `ROADMAP.md` bullet to Done.

After step 6 the `orchestrator/` tree is empty and removed.

## Open questions

- **REAPI parity.** Confirm Bazel-native RBE against Buildbarn
  covers everything the orchestrator's per-element Action
  submission did — specifically per-platform executor routing
  via `reapi_properties`. write-a's `--platforms-json` ignores
  that field today; it has to land on the Bazel
  `exec_properties` side before step 6.
- **What is a "run" for the regression diff?** `internal/regression`'s
  `LoadRun` reads `orchestrate`'s `converted.json` /
  `failures.json` / `determinism.json`. The write-a path's
  equivalent "run output" is the rendered project A/B plus the
  Bazel build result. Step 3 has to define that schema before
  diff / history can be repointed.
- **`allowlistreg` vs `readpaths`.** Both track per-element
  cmake-read paths. Determine in step 5 whether `allowlistreg`
  re-homes whole or partly collapses into `internal/readpaths`.
