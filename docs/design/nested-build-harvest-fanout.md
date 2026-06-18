# Nested-build harvest fan-out (design)

**Status: planned, not implemented.** This doc is the design for
parallelizing the nested-cmake harvest so the per-node traced
re-configures stop running strictly one-after-another. The serial code
is in `converter/cmd/convert-element-cmake/nested_pass.go`; the roadmap
bullet is under `ROADMAP.md` "Later". Delete this doc once the fan-out
lands (git history keeps the record).

## Why this is the right layer

`convert-element-cmake` wall time is dominated by cmake **subprocess
wait**, not the converter's own Go work (a CPU profile of the in-process
path is ~77% trace-JSON parsing — already cached + parallelized — and
string/path comparison is ~0.3% of wall; see git history for the
profiling that established this). So the wall-time levers are the cmake
configures, not Go-side micro-optimizations. The per-config bake
configures were the first such lever (now launched concurrently with
lowering). The nested-build harvest is the next: a chain of cmake
re-configures that today run serially on the critical path.

## What runs today, and where

`harvestNestedBuilds` (`nested_pass.go:50`) is invoked from
`runCoalescedWarmPass` (`warm_pass.go:88`), mid-`runLowerPasses`, between
pass-1 and the final re-lower — so its subprocess waits are squarely on
the critical path. It is a **serial depth-first tree walk**:

```
harvestNestedBuilds(rels):                  # rels = top-level nested dirs, sorted
  for rel in rels:                          # SIBLINGS — independent
    runNestedTraceReconfigure(nb)           # cmake subprocess — the wall cost
    fileapi.Load(nb) ; ninja.ParseFile(nb)  # cheap, in-process
    harvestNestedDescendants(nb, depth=1):  # recurse:
      kids = DetectNestedConfigures(parent.TraceRaw)
      for rel in kids:                       # SIBLINGS — independent
        StageFileAPIQueries(dir)
        runNestedTraceReconfigure(dir)       # cmake subprocess
        recurse(depth+1)
```

The wall cost is the `runNestedTraceReconfigure`
(= `cmakerun.TraceReconfigure`) calls — one per node across the whole
tree — executed strictly in sequence. For a superbuild with *K*
independent sibling subprojects, that's `sum(K)` configures of latency
where it could be `max(K)`.

## The dependency shape: a parallel tree traversal

- **Siblings at every level are independent.** Distinct build dirs,
  distinct cmake processes, no shared state except the cycle guard
  (addressed below). They can run concurrently.
- **Parent → child is a genuine data dependency.** A grandchild dir only
  exists / is refreshed *because* the parent's traced re-configure
  re-ran the parent's configure, which execs the nested cmake. The code
  relies on exactly this — grandchildren are re-configured *directly*
  rather than via a parent re-run "because its cache is warm — the
  parent's traced re-configure just re-ran its configure"
  (`nested_pass.go:111-114`). So a child's reconfigure must follow its
  parent's.

Conclusion: **fan out across siblings, stay serial along each root→leaf
path.** This is a standard parallel tree traversal — each node, once its
parent has produced its trace, spawns its children concurrently and joins
them before returning its own `NestedBuildInput`.

## Four correctness constraints

This is materially more delicate than the per-config bake fan-out
(`per_config_bake.go`), because of shared recursive state and an
ordering contract on the output.

### 1. The cycle guard becomes a thread-safe atomic claim

`visited` (`map[string]bool`) is the loop-break for superbuild cycles
(A configures B, B configures A). Today it is check-then-set across the
recursion (`nested_pass.go:56-61, 140-148`). Even parallelizing *only*
the top-level loop races it, because each top-level subtree's recursion
writes it.

Replace it with a small type whose single operation is an **atomic
claim**:

```go
type buildDirClaims struct {
    mu sync.Mutex
    m  map[string]bool
}
// claim reports whether canon was newly claimed (true) or already taken
// (false → the caller skips it, same loop-break semantics as today).
func (c *buildDirClaims) claim(canon string) bool {
    c.mu.Lock()
    defer c.mu.Unlock()
    if c.m[canon] {
        return false
    }
    c.m[canon] = true
    return true
}
```

The top-level pre-seed (outer dir + all top-level dirs) becomes a series
of `claim()` calls done *before* the fan-out. Every descendant
check-then-set site becomes `if !claims.claim(canon) { skip }`. First
claimant wins; the others skip — byte-identical to today's guard, minus
the race.

### 2. Deterministic gather is mandatory

`lowerNestedBuilds` merges `opts.NestedBuilds` **in slice order**
(`nested_cmake.go:369`), and that order flows into emitted BUILD target
order. A completion-order append would make the output depend on
scheduler timing — a silent non-determinism bug.

So results must land in **index-keyed slots** by sorted-rel position, not
`append` in completion order — the `results[i]` pattern from
`per_config_bake.go`. The same applies to `parent.Children`: pre-size it
to `len(rels)` and write child `i` into slot `i`, then (optionally)
compact out the skipped/failed slots while preserving order.

### 3. Per-node buffered stderr

Concurrent cmake subprocesses plus the harvest's many
`fmt.Fprintf(os.Stderr, "…warning…")` calls would interleave into
garbage. `TraceReconfigure` already takes `stdout, stderr io.Writer`
(`run.go:299`), so give each node its own `bytes.Buffer`, and flush the
buffers in sorted order after the join — exactly what
`runPerConfigBakes` does. Success stays quiet; the per-node warnings and
cmake diagnostics print grouped and in deterministic order.

### 4. Bounded concurrency

A wide or deep superbuild could otherwise spawn many concurrent cmake
configures at once; each runs compiler/feature probes (CPU- and
memory-heavy). Bound the fan-out with a counting semaphore. The codebase
already has this exact pattern in three places to copy from:
`lower.go:7046` (Fortran module scan), `emit/bazel/split.go:227`
(split-package emit), `lower/cmake_script_prewarm.go:187` (script
prewarm). A `runtime.NumCPU`-sized semaphore shared across the *whole*
tree traversal (not per-level) keeps the total in-flight configure count
bounded regardless of tree shape.

## Sketch of the parallel traversal

A single shared semaphore + claims guard, threaded through the recursion;
each level fans its siblings out and joins:

```go
func harvestNestedBuilds(ctx, a, hostBuildDir, rels, sink) []NestedBuildInput {
    claims := newBuildDirClaims()
    claims.claim(canonicalBuildDir(hostBuildDir))
    for _, rel := range rels {
        claims.claim(canonicalBuildDir(join(hostBuildDir, rel)))
    }
    sem := make(chan struct{}, runtime.NumCPU())
    results := make([]*NestedBuildInput, len(rels))   // index-keyed
    var wg sync.WaitGroup
    for i, rel := range rels {
        wg.Add(1)
        go func(i int, rel string) {
            defer wg.Done()
            sem <- struct{}{}; defer func() { <-sem }()
            results[i] = harvestOne(ctx, a, hostBuildDir, rel, sink, claims, sem)
        }(i, rel)
    }
    wg.Wait()
    return compactInOrder(results)   // drop nils (skips/failures), keep order
}
```

`harvestOne` does the reconfigure + load + `harvestNestedDescendants`,
and the descendant loop uses the *same* `claims` and `sem`, fanning its
siblings out the same way and writing into a pre-sized `Children` slice.
Because parent→child is serial (the child goroutine starts only after
the parent's `harvestOne` has the parent trace), the dependency is
respected by construction; only siblings overlap.

A note on the semaphore + recursion: a parent holds a semaphore slot
while waiting for its children, who also want slots — a classic
bounded-pool-with-nested-acquire that can deadlock if the pool is
exhausted by waiters. Avoid it by **releasing the parent's slot before
joining its children** (the parent's own cmake is already done by then;
it's only gathering), or by sizing the traversal so a node holds a slot
only for the duration of its *own* `TraceReconfigure`, not while waiting
on descendants. The implementation must pick one explicitly and comment
it.

## Verification plan

- `go test -race ./converter/cmd/convert-element-cmake/...` with a fixture
  that has ≥2 sibling nested builds and a 2-deep chain, asserting the
  lowered IR / emitted BUILD is **byte-identical** to the serial harvest
  across many runs (determinism gate).
- The existing nested-cmake render gate(s) under `scripts/` (the
  superbuild / nested-cmake meta gates) must stay green.
- `--out-timings` should show the nested harvest's wall drop from
  `sum(siblings)` toward `max(siblings)` on a multi-subproject superbuild
  (the `warm_configure_seconds` bucket, which is where the harvest's
  reconfigures are accounted).

## Demand signal

Only superbuild / configure-time-nested-cmake projects exercise this at
all (ExternalProject-style bootstraps, `execute_process` nested cmake);
for everything else `rels` is empty and the harvest is a no-op. Land it
when a survey member's `--out-timings` shows the nested reconfigures (in
`warm_configure_seconds`), not the fresh configure, as a meaningful slice
of wall — otherwise the concurrency surface isn't paying for itself.
