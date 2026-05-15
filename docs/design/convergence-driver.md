# Convergence driver — the fixpoint loop

`tools/converge.sh` is the uniform driver loop the ROADMAP
cross-element configure-step bootstrap describes. It manages
the round-2 AC rendezvous as a fixpoint over the `.bst`
dependency DAG: each round runs project A's trace_loads + the
converter genrules, stages outputs into project B, identifies
elements still on the miss-side of the rendezvous (their
`trace_load` returned the "miss" marker), runs their
`trace_build` targets to publish the trace + config bundle,
bumps `CONVERGE_GENERATION`, and retries.

Termination is when every `trace_load` marker reports "hit" —
no element remains on the frontier.

## Why a driver at all

After PR-1 (action-time `trace_load`) the per-element AC
lookup runs as a normal Bazel action with hermetic inputs.
Bazel's ActionCache rules apply: the action re-runs only when
its inputs change. But the AC contents (what the REAPI store
holds) can shift between dev sessions, between machines, or
between the same machine's rounds — without any input file
changing.

So we need a side-channel signal that says "the AC view has
likely changed; re-run trace_load actions." That signal is
`CONVERGE_GENERATION`, passed via `--action_env`. Bazel's
ActionCache tracks `--action_env` values as inputs; bumping
`CONVERGE_GENERATION` invalidates every trace_load action's
cached output and forces a re-query.

The driver owns the bumping. Each round of the loop:

1. Build project A's trace_load targets with the bumped
   generation — forces re-query.
2. Build project A's converter genrules — they consume the
   newly materialized trace_load outputs.
3. Stage project A's converted `BUILD.bazel.out` files into
   project B (`cmd/stage-b`).
4. Read the trace_load markers to find the frontier — elements
   whose AC lookup returned "miss".
5. For each frontier element, build the matching trace_build
   target in project B. The action runs configure / build /
   install / publish; the publish step lands the trace +
   config bundle in CAS.
6. Bump `CONVERGE_GENERATION`. Goto 1.

Termination criterion: the frontier is empty. The criterion is
guaranteed to be reached in bounded rounds — the `.bst` graph
is a DAG (BuildStream rejects cycles), and each round resolves
at least one frontier element (its dep closure is already
converged, so its trace_build can run, so its next-round
trace_load will hit). Worst case: the longest
configure-needing chain in the graph. In practice, pure-cmake
subgraphs converge in one round; alternating cmake → trace →
cmake → trace chains take depth-many rounds.

## What the driver IS NOT

- It is not a scheduler. Bazel still does all action scheduling
  within each pass. The driver just sequences passes.
- It is not REAPI-aware. `CAS_GRPC_ADDR` is opaque to the
  driver; it's threaded through to action envs and the
  publish/lookup tools handle it.
- It is not aware of element kinds. The `trace_load` /
  `trace_build` naming convention (a queryable Bazel kind +
  tag pair) is the only dispatch surface. New kinds joining
  the trace-driven set work with no driver changes.

## The shape

```
loop:
  ROUND++
  bazel build --action_env=CONVERGE_GENERATION=$ROUND \
              --action_env=CAS_GRPC_ADDR=$CAS \
              //elements/...:*           in project A
  stage-b --in $A --out $B
  miss_markers = find $A/bazel-bin/elements -name marker \
                 | xargs grep -l '^miss'
  if empty(miss_markers): terminate
  trace_build_targets = miss_markers → element-relative trace_build labels
  bazel build --action_env=CAS_GRPC_ADDR=$CAS \
              $trace_build_targets       in project B
  if ROUND >= MAX_ROUNDS: fail
final: bazel build //... in project B
```

The trace_build target name derivation is mechanical: the
trace_load output path
`bazel-bin/elements/<elem>/<name>/marker` carries `<name>`
which is either `<elem>_trace_load` or
`<elem>_trace_load_<platform>` (multi-platform). The
corresponding trace_build target is the same `<name>` with
`_trace_load` substituted for `_trace_build`.

## Operator-friendly offline mode

`CAS_GRPC_ADDR` is optional — when empty, trace-lookup and
trace-publish both short-circuit silently. The loop still runs:
trace_load actions write miss markers, the driver builds every
trace_build target, but the publish step is a no-op. The next
round's trace_load misses again (nothing was published).
Termination is via `--max-rounds`; the driver exits non-zero
with a clear diagnostic, but the per-element BUILD shapes are
still correct (placeholder shape for refused / non-converted
elements; coarse install genrules built for every other
element).

This is the "single-machine, no shared cache" mode — equivalent
to today's `bazel build A; stage-b; bazel build B` flow, just
expressed through the driver. Operators with a shared REAPI
cache pass `CAS_GRPC_ADDR` and benefit from the cross-build
convergence.

## Reference

| File | Role |
|---|---|
| `tools/converge.sh` | the driver script |
| `cmd/stage-b/main.go` | stages project A's BUILD.bazel.outs into project B |
| `rules_buildstream_bazel//rules:traces.bzl` | `trace_load` rule with the marker output |
| `cmd/write-a/handler_*.go` | per-kind install-genrule templates that produce trace_build targets |
| `scripts/meta-converge.sh` | render-half acceptance gate (stubs bazel + stage-b) |

## Status

Shipped in the cross-element configure-step bootstrap PR
stack — see [`ROADMAP.md`](../../ROADMAP.md).

Not in v1:

- Parallel trace_build execution across the frontier. v1 builds
  the frontier as a single `bazel build $trace_build_targets`
  invocation; Bazel parallelizes the actions within that build.
  Inter-build parallelism (round N's trace_load while round N+1's
  trace_builds run for an independent subgraph) is queued as a
  later optimization once a multi-element fixture surfaces the
  win.

- Integration with the deleted orchestrator's progress
  reporting. v1 prints a round-by-round summary to stderr; a
  richer signal (per-element timing, AC hit/miss histogram)
  lands when an operator workflow needs it.
