# `kind:autotools` round-2 rendezvous via the REAPI ActionCache

`docs/three-pass-flow.md` describes the architectural arc that
shapes how `kind:autotools` elements convert to Bazel. The summary
relevant here:

- **Pass 1** — `cmd/write-a` parses `.bst` graph, renders project
  A and project B.
- **Pass 2** — `bazel build` of project A runs the per-element
  converter genrule. Output is `BUILD.bazel.out` plus sidecars.
- **Pass 3** — `bazel build` of project B runs the per-element
  install genrule. Output is `install_tree.tar` (and friends).

Round-1 of `kind:autotools` runs the converter inline at pass-3:
configure / make / make-install under `build-tracer`, then the
converter consumes the trace and emits `BUILD.bazel.out` as a
sibling output of the install action. Round-2 splits that — the
converter moves to pass-2, the install genrule stops running it
inline — but doing so needs a way for pass-2 to learn that pass-3
has produced a usable trace for this element's srckey.

This doc covers the rendezvous mechanism: how pass-3 publishes,
how pass-2 looks up, and why the recipe lives where it does.

## The shape

```
write-a --autotools-round2  →  project A: per-element converter genrule
                                            + per-element trace_load target
                                project B: per-element trace_build genrule
                                            (configure + make + install
                                             + build-tracer + inline publish)

bazel build A//<elem>:<elem>_build
   pass-2:
     :<elem>_trace_load action  (rules/traces.bzl trace_load rule)
       → cmd/trace-lookup --srckey=<hex> --out-trace=... --out-make-db=...
                          --out-empty-marker=...
         → SyntheticActionDigest(srckey)
         → AC.GetActionResult(synthetic_key)
         → AC hit  ⇒ MaterializeDirectory writes trace.log (+ make-db.txt)
                     into the declared outputs; marker = "hit\n"
         → AC miss ⇒ zero-byte trace.log (+ make-db.txt) + marker = "miss\n"
     converter genrule action (srcs include :<elem>_trace_load)
       → convert-element-trace --trace-dir=<staged>
         → trace.log non-empty ⇒ emit cc_library / cc_binary
         → trace.log empty     ⇒ emit placeholder BUILD.bazel.out

bazel build B//<elem>:<elem>_trace_build
   pass-3 trace_build genrule (tagged "trace_build"):
     configure / make / make-install under build-tracer
     post-process make-db (sed filter)
     trace-publish:
       canonicalize trace + filter make-db (defense-in-depth)
       cas.UploadDir(<staged dir>)        → root Directory digest
       AC.UpdateActionResult(synthetic_key,
                             ActionResult{output_directories: [
                                 {root_directory_digest: <digest>}]})
```

The pass-3 genrule is named `<elem>_trace_build` (renamed from
the historical `<elem>_install`) and tagged with `trace_build` so
the convergence driver can query the set:
`bazel query 'attr(tags, trace_build, //elements/...:*)'`.
Cross-element consumers still reference
`//elements/<dep>:install_tree.tar` (a stable filegroup
introduced by the multi-platform install-fan-out work) rather
than the genrule name directly, so the rename is internal — no
operator-visible BUILD label changes for consumers.

The action-time `trace_load` rule (added in the trace_load /
trace_build refactor — see ROADMAP) replaced the legacy load-time
`_trace_repo` repository rule. Operationally:

- **Before:** every `bazel build A` re-ran the repo rule at load
  time before analysis started; the lookup result symlinked into
  bb_clientd / cas-fuse via `<mount>/blobs/directory/<digest>` and
  appeared as an external `@trace_<elem>//:trace` fileset. The
  analysis cache invalidated whenever the AC view shifted between
  driver passes.
- **After:** the lookup runs as a normal Bazel action against a
  staged `//tools:trace-lookup` binary. The bytes are fetched via
  gRPC `MaterializeDirectory`, not via a FUSE mount, so the
  rendezvous no longer requires `bb_clientd` for the input side.
  Bazel's `ActionCache` controls re-runs; the convergence-driver
  forces re-querying between rounds by bumping
  `--action_env=CONVERGE_GENERATION=<n>`.

## The synthetic key

Both publisher and consumer derive their AC key from the same
srckey hex (the content of `srckey.txt`, the per-element
content-narrowed digest computed by `cmd/write-a/srckey.go`).
The key is the digest of a synthetic REAPI Action proto that's
never executed:

```go
Command{ arguments: ["cmake-to-bazel/trace-publish-marker/v1", srckey] }
Action {
  command_digest:    digest(Command)
  input_root_digest: digest(empty Directory)
  do_not_cache:      false
}
synthetic_key = digest(Action)
```

The Action is never executed — only its digest is used as a
key/value lookup index in the AC. REAPI's AC doesn't care
whether anyone ran the Action; it's just a content-addressed
store keyed by Action digest.

Stability: both `trace-publish` and `trace-lookup` call
`tracenorm.SyntheticActionDigest(srckey)` (in
`internal/tracenorm/synthkey.go`), which uses
`cas.DigestProto` — the same deterministic-marshal helper
the rest of the cache layer uses. Same srckey + same Go
build of `internal/tracenorm` ⇒ same key on every machine.

The argv0 string (`cmake-to-bazel/trace-publish-marker/v1`)
namespaces the keyspace. Bumping the trailing `v1` to `v2`
(or appending a salt) rotates every stored AC entry, which
is what we'd do if the on-disk trace schema changed enough
that older traces shouldn't satisfy newer lookups.

## Why AC + REAPI, not a sidecar JSON

Several mechanisms could host the srckey → trace mapping. The
plan prototypes considered three:

1. **Checked-in `traces.json`** updated by a `trace-push` step
   committed alongside the `.bst`. Rejected — pollutes review
   with build-output digests, no cross-team sharing.
2. **A new sidecar registry service** (small REST endpoint).
   Rejected — operational burden, another infra component to
   deploy + secure + monitor.
3. **The AC of whatever REAPI cache the team is already
   running** (buildbarn, bb_clientd-as-CAS-frontend, RBE
   provider). Picked. Zero new infra: every machine that runs
   `bazel build` against the meta-project already has access.

The publisher / consumer don't need REAPI's *execution* side —
just `GetActionResult` / `UpdateActionResult` / `FindMissingBlobs`
/ `BatchUpdateBlobs`. Existing `internal/cas` GRPCStore covers
all of these.

## Cross-node convergence

The AC is *shared infrastructure*. A fresh CI runner doesn't
have its own AC — it talks to the same one every other runner
and dev machine talks to. Concretely:

- The very first build of a given srckey **anywhere** hits
  the miss path: pass-2 lookup misses → coarse pass-3 runs →
  pass-3 publishes the trace + AC entry under
  `synthetic_key=fn(srckey)`.
- All subsequent builds of the same srckey **anywhere** —
  fresh CI runner, dev's laptop, container, a different
  region's executor — get the hit path: pass-2 lookup hits
  → fine pass-3 cc compile.
- A srckey change (graph-affecting source edit) is a fresh
  rendezvous: lookup misses for the new key, coarse pass-3
  re-runs, publishes under the new key. The old key's AC
  entry stays reachable for any developer / branch still on
  the old source — natural per-srckey isolation.

For trace-publish to land *byte-identical* AC entries from
two nodes building the same srckey, two layers of stability
are needed:

- **Trace canonicalization**: pid stripping + gcc temp-path
  placeholders + sandbox prefix substitutions. Lives in
  `internal/tracenorm/canonicalize.go` (lifted from the
  build-tracer-only location it had pre-round-2 so
  trace-publish can re-apply it defensively).
- **make-db filtering**: drop the variant lines (mtimes,
  file-count summaries, db-print timestamps). Lives in
  `internal/tracenorm/makedb.go`.

The pipeline genrule applies both inline at action time
(via `build-tracer` and a sed pipeline). `trace-publish`
re-applies them in-process before computing digests, so a
publisher whose upstream `build-tracer` is older or whose
genrule's sed filter is missing a line still lands a stable
digest.

## Eviction resilience

If the AC entry survives but the referenced trace blob gets
evicted from CAS, `trace-lookup` returns a miss (it calls
`FindMissing` after reading the AR; non-empty ⇒ blob gone ⇒
treat as miss). The coarse pass-3 then re-runs and re-publishes
under the same synthetic key. Self-healing.

## Generality

The rendezvous mechanism is kind-agnostic. `_trace_repo`,
`SyntheticActionDigest`, `trace-publish`, and `trace-lookup`
all key off a srckey-string and don't know what kind produced
the trace.

**kind:make**, **kind:makemaker**, **kind:modulebuild**,
**kind:manual**, and **kind:script** opt in by setting
`traceDrivenSrckeyPatterns` on their registered
`pipelineHandler`. `pipelineHandler.RenderA` /
`pipelineHandler.RenderB` (in `handler_pipeline.go`) check the
field via `shouldUseRound2` and dispatch to the same kind-
agnostic helpers `kind:autotools` uses
(`handler_pipeline_round2.go::renderTraceDrivenRound2A` and
`pipelineTraceExtensionRound2`).

Per-kind srckey narrowing decisions:

- **kind:make**: `Makefile` + `**/Makefile` + `**/*.h` family.
  `.c` content is path-only (recipes don't depend on it).
- **kind:makemaker**: `Makefile.PL` + `**/Makefile` + `**/*.xs`
  + `**/*.h` family. `*.pm` is path-only (pure Perl, doesn't
  drive cc); `*.c` typically generated from `*.xs` so path-only.
- **kind:modulebuild**: `Build.PL` + `**/*.xs` + `**/*.h` family.
- **kind:manual** + **kind:script**: empty rule set
  (`&readPathsPatterns{}`) → `matchesSrckeyPatterns` returns
  content-included for every file. The .bst's commands could
  be anything; there's no kind-level signal for which files
  drive build commands. Per-element narrowing is available
  via the existing read-paths.txt sibling.

Further trace-driven kinds (possibly `kind:flatpak_image`,
`kind:collect_*`) join the same way — one line in `init()`
setting `traceDrivenSrckeyPatterns`. The pattern set's job is
deciding which file paths gate the BUILD COMMANDS
(content-included) vs which only affect compile OUTPUT bytes
(path-only).

## Roll-out

Round-2 is the default whenever `--convert-element-trace`
is set; passing `--trace-round1` opts back into the legacy
single-genrule shape (project A is a marker; project B's
install genrule runs the converter inline). The opt-out exists
because some render gates assert fine-conversion-shape
properties (per-target CFLAGS, libtool dual-compile, etc.) by
running `bazel build` against round-1's inline converter; in
round-2 those properties only emerge after pass-3 has
published, which needs the AC + bb_clientd mount.

## Gates

- `scripts/meta-autotools-round2.sh` — render-half gate. Locks
  in the rendered shape (project A converter genrule, project
  B install + trace-publish). Runs without buildbarn.
- `tools/e2e-meta-autotools-round2-live.sh` — live-AC gate.
  Stands up buildbarn (and optionally bb_clientd), runs
  `trace-publish` against the real REAPI endpoint, runs
  `trace-lookup` and asserts the published digest round-trips,
  and (with bb_clientd) asserts the Directory blob is mountable
  at `<mount>/cas/<digest>/`. The same path the `_trace_repo`
  rule symlinks at load time. Wired into CI alongside the other
  buildbarn-tagged jobs (`make e2e-meta-autotools-round2-live`).

## Reference

| File | Role |
|---|---|
| `internal/tracenorm/canonicalize.go` | trace-line shaping (lifted from cmd/build-tracer) |
| `internal/tracenorm/makedb.go` | make-db variant-line drop list |
| `internal/tracenorm/synthkey.go` | `SyntheticActionDigest(srckey)` recipe |
| `cmd/trace-publish/main.go` | round-1 publisher; runs inline in pass-3 |
| `cmd/trace-lookup/main.go` | round-2 consumer; shells out from `_trace_repo` |
| `cmd/write-a/handler_pipeline_round2.go` | kind-agnostic round-2 helpers (renderTraceDrivenRound2A, pipelineTraceExtensionRound2) used by both kind:autotools and any pipelineHandler-shaped kind opting in |
| `cmd/write-a/traces_bzl.go` | renders `rules/traces.bzl` |
| `cmd/write-a/traces_json.go` | renders `tools/traces.json` |
| `scripts/meta-autotools-round2.sh` | render gate |
