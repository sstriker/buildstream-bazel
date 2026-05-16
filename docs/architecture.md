# Architecture

A descriptive map of what's actually in this repo today: the binaries
shipped, the data flowing between them, and the shared substrates each
one leans on. For what's done vs queued see [`ROADMAP.md`](../ROADMAP.md).
For a diagram-first tour of the same material see
[`docs/visual-guide.md`](visual-guide.md). For the architectural
framing (how the two-project shape, rendezvous channel, fixpoint driver,
and `finalize-b` fit together) see
[`docs/design/conversion-architecture.md`](design/conversion-architecture.md).

## Goal in one paragraph

Take a BuildStream project (the FreeDesktop SDK is the working target)
and produce a Bazel build that builds the same artifacts. The
single-element converter runs cmake against a `kind: cmake` element in
a sandbox, reads cmake's File API + ninja graph, lowers the result to
an internal representation, and emits both a `BUILD.bazel` (so Bazel
can drive the build) and a synthesized cmake-config bundle (so
downstream cmake consumers still resolve `find_package()`). The
multi-element driver is **`cmd/write-a` + Bazel**: write-a renders a
two-pass meta-project (project A runs the per-element converters as
Bazel genrules; project B is the consumer workspace built against
project A's outputs), and **Bazel** schedules the cross-element work —
including, against a Buildbarn cluster, fanning per-element conversions
out across a remote executor pool via Bazel-native `--remote_executor`.
The same two-pass model extends to non-cmake kinds — `kind:autotools`
(round-1 coarse + round-2 trace-driven), `kind:make`, `kind:makemaker`,
`kind:modulebuild`, `kind:manual`, `kind:script`, and `kind:meson`
(introspection-driven Phase A) are all shipped via the per-kind
handlers under `cmd/write-a/`. See [`ROADMAP.md`](../ROADMAP.md) for
current vs. queued state.

## Repo layout

```
converter/                  single-element converter (the per-package brain)
  cmd/convert-element-cmake/      CLI entry point (cmake)
  cmd/convert-element-meson/  CLI entry point (meson; introspection-driven, see docs/design/meson-native-render.md)
  cmd/derive-toolchain/     emits cc_toolchain + toolchain.cmake from a cmake probe
  internal/cli              flag parsing + exit codes
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

cmd/                        the write-a + Bazel driver, its tooling, and
                            the re-homed analysis tools
  write-a/                  meta-project renderer (per-kind handlers) — the multi-element driver
  stage-b/                  stages project A's BUILD.bazel.out into project B; reports the changed-element set
  run-manifest/             snapshots a built project A into the run-manifest shape regression diffs on
  build-tracer/             native ptrace + strace fallback; --source-root opts in to openat capture
  trace-publish/            publishes canonicalized trace+make-db AC entry under SyntheticActionDigest(srckey)
  trace-lookup/             A-side AC reader the rules_buildstream_bazel trace_load rule shells to (action-time)
  finalize-b/               post-convergence strip pass — converted project B → standalone Bazel project
  convert-element-trace/    trace + `make -np` → native cc rules (trace-driven kinds)
  audit-narrowing/          patterns × oracle → undercoverage report
  build-cc-index/ relax-keeps/  gazelle-roundtrip support (Phase 7/8b)
  source-push/              packs a source tree into CAS via the REAPI wire format
  bst-translate/            rewrites .bst sources to kind:remote-asset
  orchestrate-diff/         compares two runs; exit 2 on regression
  orchestrate-history/      queries fingerprint history for churn / drift

internal/                   shared substrates
  cas                       local content-addressable store, CAS interface (incl. REAPI AC surface)
  fidelity                  symbol-set + behavioral diffs (used by tests)
  manifest                  per-package + per-run JSON schemas
  shadow                    path-only-stat shadow-tree creator + read-path tracer
  readpaths                 shared pattern matcher for write-a + audit-narrowing
  tracenorm                 canonicalize / openat filtering / SyntheticActionDigest
  element                   .bst project loader, dep graph, kind filtering
  synthprefix               per-element CMAKE_PREFIX_PATH stub trees
  sourcecheckout            resolves source spec → local tree (local/git/remote-asset)
  exports                   parse <Pkg>Targets.cmake → imports manifest
  regression                run-vs-run diff, fingerprint registry
  allowlistreg              per-package shadow-tree allowlist registry
  bsttranslate              .bst rewrites to kind:remote-asset

tools/bst                   BuildStream-style CLI wrapper around write-a (`bst build` muscle memory)
tools/converge.sh           fixpoint driver — orchestrates the round-2 AC rendezvous to convergence

rules_buildstream_bazel/    in-repo Bazel module referenced by rendered project A + project B
  rules/traces.bzl          trace_load rule (action-time AC consumer of the round-2 rendezvous)
  rules/zero_files.bzl      zero-byte stub materializer (shadow-tree primitive)
  rules/sources.bzl         sources module extension (CAS-FUSE-backed external repos)

deploy/buildbarn/           local-dev REAPI cluster
  docker-compose.yml        bb-storage + bb-scheduler + bb-worker + bb-runner-bare
  config/*.jsonnet          per-service configs
  runner/Dockerfile         custom bb-runner image with cmake/ninja

scripts/ tools/             render-gate scripts + maintenance helpers
docs/                       milestone plans, schema docs, known-deltas
.github/                    CI workflow + post-failure-tail composite action
```

## The two binaries

### `convert-element-cmake`

Single-package converter. Given an extracted source root + cmake build
options, produces a directory containing `BUILD.bazel`, a
`<Pkg>Config.cmake` + `<Pkg>Targets.cmake` + `<Pkg>Targets-Release.cmake`
bundle, and a `manifest.json` describing the element and its outputs.

Pipeline, in order:

1. **CLI / env setup** — `converter/internal/cli` parses flags;
   `cmakerun.Configure` scrubs the environment to a known whitelist
   (empty `HOME`, fixed locale, `SOURCE_DATE_EPOCH`) before exec'ing
   cmake. Per-action sandboxing comes from Bazel's spawn strategy at
   the genrule layer, not from a wrapper inside the converter.
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
   any). `--out-timings` optionally records per-phase wall-clock
   (cmake configure / translation / total).

Tiered failures land in `converter/internal/failure/failure.go`.
Tier-1 (`unsupported-target-type`, `configure-failed`,
`unresolved-include`, …) means "this element can't convert" without
aborting; Tier-2/3 are hard errors.

`derive-toolchain` is a sister binary that runs cmake against a tiny
probe project and emits a `cc_toolchain_config.bzl` + `BUILD.bazel`
for downstream Bazel consumers, plus a `toolchain.cmake` that
pre-populates cmake's compiler-probe cache so per-element conversions
skip the expensive probe.

### `write-a`

Multi-element driver. Given a `.bst` graph, renders a **two-pass
meta-project** and lets **Bazel** schedule the cross-element work.

- **Project A** (the meta workspace): one per-element genrule that
  invokes the per-kind converter (`convert-element-cmake` for
  `kind:cmake`, `convert-element-trace` + `build-tracer` for the
  trace-driven kinds, `convert-element-meson` for `kind:meson`, …).
  `bazel build` in project A runs those genrules; each emits a
  `BUILD.bazel.out` (the converted rules) plus, for `kind:cmake`,
  a `cmake-config-bundle.tar`. Cross-element `kind:cmake` deps are
  staged on `CMAKE_PREFIX_PATH` inside the consumer's genrule so
  `find_package()` resolves.
- **Staging** — `cmd/stage-b` copies each element's `BUILD.bazel.out`
  from project A's `bazel-bin` over project B's
  `elements/<name>/BUILD.bazel`, and reports the set of elements whose
  staged content actually changed (a content diff — the "what just
  re-converted" signal).
- **Project B** (the consumer workspace): the staged per-element
  `BUILD.bazel`s plus the element source trees. `bazel build` in
  project B compiles the converted `cc_*` rules; cross-element labels
  are `//elements/<name>:<target>` and resolve within the module.

The per-kind dispatch lives in `cmd/write-a/` (one handler file per
kind). `tools/bst` is a BuildStream-style CLI wrapper so `bst build`
muscle memory keeps working through the conversion. The
re-homed analysis tools — `orchestrate-diff` (compares two runs,
exit 2 on regression) and `orchestrate-history` (queries
`internal/regression`'s fingerprint registry for churn / drift) — and
`bst-translate` (rewrites `.bst` sources to `kind:remote-asset`) sit
beside write-a under `cmd/`; they came out of the now-deleted
orchestrator in the absorption (see
[`docs/design/orchestrator-absorption.md`](design/orchestrator-absorption.md)).

## Shared substrates

### `internal/cas`

Local content-addressable store with an interface that matches the
REAPI CAS shape (`FindMissing`, `BatchUpdate`, `BatchRead`,
`Read`/`Write` for streaming). `cmd/source-push` uses it to pack
source trees into a real Buildbarn CAS over the REAPI wire format;
`cmd/trace-publish` / `cmd/trace-lookup` use its Action-cache surface
(`UpdateActionResult` / `GetActionResult`) for the round-2
trace-rendezvous wire contract. (REAPI *execution* — submitting
per-element conversions as Actions — was the orchestrator's job,
implemented in a since-deleted `internal/reapi` package; the write-a +
Bazel driver drives remote execution through Bazel's own native REAPI
client instead. See
[`docs/design/orchestrator-absorption.md`](design/orchestrator-absorption.md).)

### `internal/manifest`

The imports-manifest schema + resolver: `<Pkg>Targets.cmake`-derived
`Element` / `Export` records mapping out-of-tree cmake targets to
`//elements/<name>:<target>` Bazel labels, which the converter
consumes when a `find_package()` resolves to another converted
element. write-a renders `MODULE.bazel` for projects A and B, making
each a self-contained bzlmod project; cross-element `BazelLabel`s in
the
per-element imports manifests are `//elements/<name>:<target>`-shaped.

### `internal/shadow`

Path-only-stat shadow-tree creator. Mirrors the source root with
zero-byte stubs except for files matching the per-package allowlist
(default: `CMakeLists.txt`, `*.cmake`, `*.in`, `*.txt`, augmented per
package). cmake's `--trace-expand` JSON output identifies the
read-paths the converter actually saw, so a run's
`read_paths.json` records every file the conversion was sensitive
to. The `internal/shadow/trace.go` parser handles that; the per-
package allowlist registry lives in
`internal/allowlistreg`.

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
or both): a JSON array from `convert-element-cmake`'s
`--out-cmake-configure-reads` (cmake-side, sourced from
build.ninja's `RERUN_CMAKE` deps), and a canonicalized
`trace.log` from `build-tracer --source-root=...` (trace-driven
side, sourced from openat capture). Recipe + scope:
[`docs/design/narrowing-audit.md`](design/narrowing-audit.md).

### `internal/fidelity`

Symbol-tier and behavioral-tier diff library. `DiffSymbols` compares
`SymbolSet`s extracted via `nm`/`objdump`; `DiffBehavior` runs a
test binary under both build paths and compares stdout/stderr/exit.
Used by `converter/cmd/convert-element-cmake/fidelity_e2e_test.go`,
the fidelity gate (parameterized over fixtures — hello-world for
smoke, fmt for real-world). Not a runtime gate on conversion — only
a test.

## Downstream Bazel envelope

write-a's project B is a self-contained bzlmod project: a rendered
`MODULE.bazel` declaring `bazel_dep` on `rules_cc`, each converted
element at `elements/<name>/BUILD.bazel`, with the element's source
tree alongside so the converter's relative-path `srcs`/`hdrs`
resolve. Cross-element labels are `//elements/<name>:<target>` and
resolve directly within the module.

`scripts/meta-cross-cmake.sh` is the downstream-build gate: it renders
a cross-element `kind:cmake` graph, builds project A, `stage-b`'s into
project B, and `bazel build`s the consumer there — the cross-element
converted `cc` deps link end to end. The full two-pass round-trip
including a smoke binary is `scripts/meta-hello.sh`.

## Build / test targets

`Makefile` is the dev surface. The shapes that matter:

- `make` — builds the Go binaries into `build/bin/`.
- `make test` — unit tests (no cmake required; pre-recorded fixtures).
- `make e2e-hello-world` / `make e2e-fmt` — converter e2e against
  checked-in / fetched cmake projects (build tag `e2e`).
- `make e2e-fidelity` / `make e2e-fidelity-fmt` — symbol-equivalence
  fidelity gate (cmake reference vs convert-element-cmake + bazel).
- `make e2e-cmake-consumer` — downstream `find_package()` resolves a
  converted element's synthesized cmake-config bundle.
- `make e2e-toolchain-skip` — derive-toolchain configure-skip gate.
- `make e2e-meta-*` — the write-a render + two-pass-build gates, one
  per kind / shape (`meta-hello`, `meta-cross-cmake`, `meta-stack`,
  the autotools / meson / pyproject families, …).
- `make e2e-meta-regression` — run-vs-run regression gate
  (`run-manifest` snapshots a built project A; `orchestrate-diff`
  diffs two runs for output drift).
- `make e2e-meta-buildbarn-re` — write-a + Bazel + real Buildbarn
  remote-execution + build-without-the-bytes gate.

`.github/workflows/ci.yml` is the CI surface. Four jobs: `unit`,
`e2e` (cmake), `bazel-e2e`, `buildbarn-e2e`. Each step pipes
output into `/tmp/cijob.log`; the
`.github/actions/post-failure-tail` composite action posts the
last 150 lines to the PR on failure.

## Deployment substrate (local dev)

`deploy/buildbarn/docker-compose.yml` brings up bb-storage,
bb-scheduler, bb-worker, and bb-runner-bare. The runner is a custom
image (`deploy/buildbarn/runner/Dockerfile`) that layers cmake and
ninja onto upstream's distroless `bb-runner-bare` at the pinned
versions (currently 3.28.3 / 1.11.1, matching
`deploy/buildbarn/config/worker.jsonnet`'s advertised platform
properties). Per-service jsonnet configs live in
`deploy/buildbarn/config/`.

This stack is the local-dev REAPI substrate. `make e2e-meta-buildbarn-re`
points a rendered project A's converter genrule at it via Bazel-native
`--remote_executor` and asserts the genrule executes on a real worker
build-without-the-bytes; `make e2e-source-push` and
`make e2e-meta-autotools-round2-live` exercise the CAS / AC wire
contracts against it. A production deployment is the same shape at
scale: point `bazel build`'s `--remote_executor` / `--remote_cache` at
the real cluster.

## Where to start reading

If you're new and want a single thread through the codebase:

1. `converter/cmd/convert-element-cmake/main.go` — the converter pipeline
   in 80 readable lines.
2. `converter/internal/lower/lower.go` — where most converter logic
   actually lives.
3. `cmd/write-a/main.go` — the multi-element driver: `.bst` graph in,
   two-pass meta-project out.
4. `scripts/meta-hello.sh` — the full two-pass round-trip (write-a →
   `bazel build` A → `stage-b` → `bazel build` B → smoke binary) as a
   readable shell script.
5. `converter/cmd/convert-element-cmake/fidelity_e2e_test.go` — the
   e2e test that proves the stack produces the same artifacts cmake
   would.
