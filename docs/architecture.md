# Architecture

A descriptive map of what's actually in this repo today: the binaries
shipped, the data flowing between them, and the shared substrates each
one leans on. For what's done vs queued see [`ROADMAP.md`](../ROADMAP.md).
For a diagram-first tour of the same material see
[`docs/visual-guide.md`](visual-guide.md).

## Goal in one paragraph

Take a BuildStream project (the FreeDesktop SDK is the working target)
and produce a Bazel build that builds the same artifacts. The pipeline
runs cmake against each `kind: cmake` element in a sandbox, reads
cmake's File API + ninja graph, lowers the result to an internal
representation, and emits both a `BUILD.bazel` (so Bazel can drive the
build) and a synthesized cmake-config bundle (so downstream cmake
consumers still resolve `find_package()`). Every element's outputs are
content-addressed in CAS; an orchestrator coordinates the multi-element
graph, materializes cross-element source/dependency trees, and
optionally submits the per-element conversion as a REAPI Action so a
remote Buildbarn cluster can fan out the work. The trace-driven
two-pass meta-project shape (`cmd/write-a` + Bazel-as-orchestrator)
extends the same conversion model to non-cmake kinds — `kind:autotools`
(round-1 coarse + round-2 trace-driven), `kind:make`, `kind:makemaker`,
`kind:modulebuild`, `kind:manual`, `kind:script`, and `kind:meson`
(introspection-driven Phase A) are all shipped via the per-kind
handlers under `cmd/write-a/`. See [`ROADMAP.md`](../ROADMAP.md) for
current vs. queued state.

## Repo layout

```
converter/                  single-element converter (the per-package brain)
  cmd/convert-element/      CLI entry point (cmake)
  cmd/convert-element-meson/  CLI entry point (meson; introspection-driven, see docs/design/meson-native-render.md)
  cmd/derive-toolchain/     emits cc_toolchain + toolchain.cmake from a cmake probe
  internal/cli              flag parsing + exit codes
  internal/hermetic         bwrap argv builder, env scrubbing
  internal/cmakerun         drives `cmake --trace-expand`, drops File API queries
  internal/fileapi          codemodel-v2 / toolchains-v1 / cmakeFiles-v1 parsers
  internal/ninja            build.ninja parser (custom, ~400 lines)
  internal/lower            File API + ninja → IR (the brain)
  internal/ir               IR types: Package, Target, Source, Genrule, ImportedTarget
  internal/emit/bazel       IR → BUILD.bazel
  internal/emit/cmaketoolchain  Model → toolchain.cmake (probe-skip cache)
  internal/emit/bazeltoolchain  Model → cc_toolchain_config.bzl + toolchains
  internal/toolchain        cmake probe + variant fold (Observe)
  internal/failure          failure.json schema + Tier 1 classifiers

orchestrator/               multi-element driver
  cmd/orchestrate           main entry point
  cmd/orchestrate-bst-translate  rewrites .bst sources to kind:remote-asset
  cmd/orchestrate-diff      compares two runs; exit 2 on regression
  cmd/orchestrate-history   queries fingerprint history for churn / drift
  internal/element          .bst project loader, dep graph, kind filtering
  internal/orchestrator     concurrency loop, AC/CAS layer, REAPI submit path
  internal/sourcecheckout   resolves source spec → local tree (local/git/remote-asset)
  internal/bsttranslate     .bst rewrites to kind:remote-asset
  internal/synthprefix      per-element CMAKE_PREFIX_PATH stub trees
  internal/exports          parse <Pkg>Targets.cmake → imports manifest
  internal/regression       run-vs-run diff, fingerprint registry
  internal/allowlistreg     per-package shadow-tree allowlist registry

internal/                   shared substrates (used by both binaries)
  cas                       local content-addressable store, CAS interface
  reapi                     REAPI Action submission (Executor, GRPCExecutor)
  fidelity                  symbol-set + behavioral diffs (used by tests)
  manifest                  per-package + per-run JSON schemas
  shadow                    path-only-stat shadow-tree creator + read-path tracer
  readpaths                 shared pattern matcher for write-a + audit-narrowing
  tracenorm                 canonicalize / openat filtering / SyntheticActionDigest

cmd/                        write-a-side binaries + the trace-driven half
  build-tracer/             native ptrace + strace fallback; --source-root opts in to openat capture
  trace-publish/            publishes canonicalized trace+make-db AC entry under SyntheticActionDigest(srckey)
  trace-lookup/             A-side load-time AC reader for round-2 _trace_repo
  audit-narrowing/          patterns × oracle → undercoverage report
  write-a/                  meta-project renderer (per-kind handlers)

deploy/buildbarn/           local-dev REAPI cluster
  docker-compose.yml        bb-storage + bb-scheduler + bb-worker + bb-runner-bare
  config/*.jsonnet          per-service configs
  runner/Dockerfile         custom bb-runner image with cmake/ninja/bwrap

tools/                      maintenance scripts (not on the runtime path)
  fixtures/                 record-fileapi.sh + scale-fixture generator
  audit/                    misc one-off audit helpers
  install-bazelisk.sh       local-dev bazel bootstrap

docs/                       milestone plans, schema docs, known-deltas
.github/                    CI workflow + post-failure-tail composite action
```

## The two binaries

### `convert-element`

Single-package converter. Given an extracted source root + cmake build
options, produces a directory containing `BUILD.bazel`, a
`<Pkg>Config.cmake` + `<Pkg>Targets.cmake` + `<Pkg>Targets-Release.cmake`
bundle, and a `manifest.json` describing the element and its outputs.

Pipeline, in order:

1. **CLI / hermetic setup** — `converter/internal/cli` parses flags,
   `converter/internal/hermetic` builds a `bwrap` argv that scrubs the
   environment to a known whitelist.
2. **`cmake --trace-expand` probe** —
   `converter/internal/cmakerun/run.go` drops File API query stamps
   into the build dir and runs cmake. The trace JSON is the
   read-path source of truth for the shadow-tree allowlist.
3. **File API replay** —
   `converter/internal/fileapi` walks the reply directory and parses
   `codemodel-v2` (targets, sources, link/compile fragments),
   `toolchains-v1` (compiler ID, flags, builtin paths), and
   `cmakeFiles-v1` (read-paths cmake itself relied on).
4. **Ninja graph** — `converter/internal/ninja/parse.go` parses
   `build.ninja` for the custom-command subset that the codemodel
   undermarks. Mostly used to fish out genrules.
5. **Lower** — `converter/internal/lower/lower.go` is the brain.
   It turns the typed File API + ninja outputs into
   `converter/ir/types.go` (`Package`, `Target`, `Source`,
   `Genrule`, `ImportedTarget`). Most converter bugs land here.
6. **Emit** — `converter/emit/bazel/emit.go` renders the
   IR as a `BUILD.bazel` (with `load("@rules_cc//cc:defs.bzl", …)`),
   and `converter/internal/emit/cmaketoolchain` /
   `converter/internal/emit/bazeltoolchain` emit the cmake bundle and
   the cc_toolchain rules respectively.
7. **Manifest** — `internal/manifest` writes `manifest.json` (sha256
   of every output, the toolchain fingerprint, the failure tier if
   any).

Tiered failures land in `converter/internal/failure/failure.go`.
Tier-1 (`unsupported-target-type`, `configure-failed`,
`unresolved-include`, …) means "this element can't convert; the run
continues without it." Tier-2/3 abort the orchestrator run.

`derive-toolchain` is a sister binary that runs cmake against a tiny
probe project and emits a `cc_toolchain_config.bzl` + `BUILD.bazel`
for downstream Bazel consumers, plus a `toolchain.cmake` that
pre-populates cmake's compiler-probe cache so per-element conversions
skip the expensive probe.

### `orchestrate`

Multi-element driver. Given a BuildStream project root and an output
directory, walks the element graph and runs one converter per
`kind: cmake` element in topological order, then writes a top-level
`converted.json` manifest.

Pipeline, in order:

1. **Element discovery** —
   `orchestrator/internal/element/project.go` reads the .bst files
   directly (no `bst` binary involved at this stage),
   `BuildGraph()` builds the dep DAG, `FilterByKind("cmake")` drops
   non-cmake elements onto the deferred list.
2. **Source resolution** —
   `orchestrator/internal/sourcecheckout` resolves each element's
   `sources:` spec to a local tree. Handles `local:`, `git:`, and
   `kind: remote-asset` (CAS-resolved). Caches under
   `--cache-dir`. `bsttranslate` is the offline cousin: rewrites .bst
   sources to `kind: remote-asset` so subsequent runs hit CAS instead
   of fresh git clones.
3. **Synth-prefix staging** —
   `orchestrator/internal/synthprefix/build.go` builds a per-element
   `CMAKE_PREFIX_PATH` tree from each dep's already-emitted cmake
   bundle. Creates zero-byte stubs at every `IMPORTED_LOCATION_<CFG>`
   path the bundle references so cmake's `find_package()` resolves
   without any actual built artifacts present.
4. **Per-element conversion** —
   `orchestrator/internal/orchestrator/run.go:processElement` is the
   per-element worker. Two execution modes:
   - **Local** (default): `convertOne()` runs `convert-element` via
     `os/exec` against the staged source root.
   - **Remote** (`--execute`): `remoteExecute()` packages the
     element's input root into REAPI inputs, submits an Action via
     `internal/reapi`, and downloads outputs from CAS.
   Either way, the per-element output directory is then ingested via
   `internal/cas` so re-runs deterministically reuse outputs.
5. **Imports manifest** —
   `orchestrator/internal/exports/extract.go` parses the freshly-emitted
   `<Pkg>Targets.cmake` and folds it into a per-element imports
   manifest the converter consumes when it sees a `find_package()`
   that resolves to another converted element.
6. **Run-level manifest** — `converted.json` records every element's
   digest + status; consumed by `bazel/converted_pkg_repo.bzl` and
   `orchestrate-diff`.

Concurrency is `--concurrency` workers over the dep DAG; each element
waits for its deps' synth-prefix to land before it starts. The
50-element scale fixture under
`orchestrator/testdata/fdsdk-scale/` exercises this loop at
concurrency=1/8/32 and asserts byte-identical output across levels.

`orchestrate-diff` and `orchestrate-history` are post-run analysis
tools: diff compares two `converted.json`s and reports newly-failed
elements (exit 2 if any), history queries
`orchestrator/internal/regression`'s fingerprint registry to surface
churn or per-element drift.

## Shared substrates

### `internal/cas`

Local content-addressable store with an interface that matches the
REAPI CAS shape (`FindMissing`, `BatchUpdate`, `BatchRead`,
`Read`/`Write` for streaming). The orchestrator uses it both as its
own per-element output cache and as the staging area for inputs it
uploads to a remote Buildbarn.

### `internal/reapi`

Thin Action-submission layer. `Executor` is the surface
(`Execute(ctx, ActionDigest) → ActionResult`); `GRPCExecutor` talks
to a real REAPI Execution service. Input-tree construction and
output-blob download are the orchestrator's job — `reapi` doesn't
expose CAS or AC clients of its own; callers reuse `internal/cas`
for that.

### `internal/manifest`

JSON schemas for per-package `manifest.json` and run-level
`converted.json`. The orchestrator's `<out>/MODULE.bazel` makes the
output directory a self-contained bzlmod project; cross-element
`BazelLabel`s in the per-element imports manifests are
`//elements/<name>:<target>`-shaped.

### `internal/shadow`

Path-only-stat shadow-tree creator. Mirrors the source root with
zero-byte stubs except for files matching the per-package allowlist
(default: `CMakeLists.txt`, `*.cmake`, `*.in`, `*.txt`, augmented per
package). cmake's `--trace-expand` JSON output identifies the
read-paths the converter actually saw, so a run's
`read_paths.json` records every file the conversion was sensitive
to. The `internal/shadow/trace.go` parser handles that; the per-
package allowlist registry lives in
`orchestrator/internal/allowlistreg`.

### `internal/readpaths`

Shared pattern matcher for read-paths and srckey narrowing
(`Rule`, `Patterns`, `Parse`, `Format`, `Match`). One
authoritative implementation used by both `cmd/write-a` (which
emits `srckey-patterns.txt` per element) and `cmd/audit-narrowing`
(which reads it back to flag undercoverage drift). cmd/write-a's
local `patternRule` and `readPathsPatterns` are aliases to
this package's `Rule` and `Patterns`. See
[`docs/design/narrowing-audit.md`](design/narrowing-audit.md).

### `cmd/audit-narrowing`

Diffs an element's narrowing patterns against an action-time
read oracle and reports paths the oracle says were read but the
patterns leave name-only. Two oracle inputs accepted (either
or both): a JSON array from `convert-element`'s
`--out-cmake-configure-reads` (cmake-side, sourced from
build.ninja's `RERUN_CMAKE` deps), and a canonicalized
`trace.log` from `build-tracer --source-root=...` (trace-driven
side, sourced from openat capture). Recipe + scope:
[`docs/design/narrowing-audit.md`](design/narrowing-audit.md).

### `internal/fidelity`

Symbol-tier and behavioral-tier diff library. `DiffSymbols` compares
`SymbolSet`s extracted via `nm`/`objdump`; `DiffBehavior` runs a
test binary under both build paths and compares stdout/stderr/exit.
Used by `orchestrator/internal/orchestrator/fidelity_e2e_test.go`,
which is the M5b acceptance gate (parameterized over fixtures —
hello-world for smoke, fmt for real-world). Not currently a runtime
gate on conversion — only a test.

## Downstream Bazel envelope

The orchestrator's `<out>/` is a self-contained bzlmod project. The
orchestrator emits `<out>/MODULE.bazel` declaring `bazel_dep` on
`rules_cc`; each converted element lives at
`<out>/elements/<name>/BUILD.bazel`, with the source root's top-level
entries symlinked at the package root so the converter's
relative-path `srcs`/`hdrs` resolve. Cross-element labels are
`//elements/<name>:<target>` and resolve directly within the module.

The bazel-build downstream e2e
(`orchestrator/internal/orchestrator/bazelbuild_test.go`) runs the
orchestrator over the FDSDK subset, then runs
`bazel build //elements/components/uses-hello:uses_hello_bin`
directly inside `<out>/`.

## Build / test targets

`Makefile` is the dev surface. The shapes that matter:

- `make` — builds all five Go binaries into `build/bin/`.
- `make test` — unit tests (no cmake required; pre-recorded fixtures).
- `make e2e-hello-world` / `make e2e-fmt` — converter e2e against
  checked-in / fetched cmake projects (build tag `e2e`).
- `make e2e-orchestrate` / `make e2e-orchestrate-scale` — orchestrator
  end-to-end and 50-element scale gate.
- `make e2e-cmake-consumer` / `make e2e-bazel-build` — downstream
  consumer gates (cmake-side and bazel-side).
- `make e2e-toolchain-skip` — derive-toolchain integration gate.
- `make e2e-fidelity` / `make e2e-fidelity-fmt` — symbol+behavioral
  fidelity gate.
- `make e2e-buildbarn` / `make e2e-buildbarn-execute` — real-Buildbarn
  cache + execute gates (require docker compose).

`.github/workflows/ci.yml` is the CI surface. Four jobs: `unit`,
`e2e` (cmake+bwrap), `bazel-e2e`, `buildbarn-e2e`. Each step pipes
output into `/tmp/cijob.log`; the
`.github/actions/post-failure-tail` composite action posts the
last 150 lines to the PR on failure.

## Deployment substrate (local dev)

`deploy/buildbarn/docker-compose.yml` brings up bb-storage,
bb-scheduler, bb-worker, and bb-runner-bare. The runner is a custom
image (`deploy/buildbarn/runner/Dockerfile`) that layers cmake,
ninja, and bubblewrap onto upstream's distroless `bb-runner-bare`
at the versions the orchestrator's `defaultPlatform` declares
(currently 3.28.3 / 1.11.1 / 0.8.0). Per-service jsonnet configs
live in `deploy/buildbarn/config/`.

This stack is what `make e2e-buildbarn-execute` exercises. It is
the local-dev REAPI substrate; production deployments would point
the orchestrator's `--execute` flag at a real cluster.

## Where to start reading

If you're new and want a single thread through the codebase:

1. `converter/cmd/convert-element/main.go` — the converter pipeline
   in 80 readable lines.
2. `converter/internal/lower/lower.go` — where most converter logic
   actually lives.
3. `orchestrator/cmd/orchestrate/main.go` and
   `orchestrator/internal/orchestrator/run.go` — the multi-element
   driver.
4. `orchestrator/internal/orchestrator/run.go`'s `writeBzlmodProject`
   — emits `<out>/MODULE.bazel` so the orchestrator output is a
   self-contained, directly-buildable bzlmod project.
5. `orchestrator/internal/orchestrator/fidelity_e2e_test.go` — the
   e2e test that proves the whole stack produces the same artifacts
   cmake would.
