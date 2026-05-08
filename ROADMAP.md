# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

- **`execute_process` recovery for kind:cmake.** Native lift
  for the deterministic buckets (`cmake -E touch / copy /
  copy_if_different`; file-producing tools with declared
  `OUTPUT_FILE`). Unliftable buckets (version stamps via
  `git rev-parse`, host probes like `uname -m / gcc --version`,
  multi-COMMAND pipelines, opaque shell scripts) emit a typed
  `unsupported-execute-process` Tier-1 failure with a per-call
  triage report (file:line, bucket, reason, argv). File-producing
  hoists carry a `cmake-codegen-execute-process-hoisted` audit
  tag flagging the configure-time → build-time move. Phase B
  (round-2 cmake fallback that routes unliftable elements
  through a coarse `cmake configure + ninja + install` genrule,
  mirroring kind:autotools/make round-2) is queued; until it
  lands, projects with unliftable calls are excluded by the
  orchestrator the same way other Tier-1 failures are. Recipe:
  `docs/research/cmake_analysis.md` §9; failure schema:
  `docs/failure-schema.md` `unsupported-execute-process`.
- **Multi-version cmake compatibility shakeout.** Per-object
  schema-major validation now lives in `fileapi/reply.go` and a
  non-blocking `e2e-latest-cmake` CI job runs the converter
  against the runner's stock cmake (3.31+) alongside the pinned
  3.28.3 path. The first surface this catches in practice is
  build.ninja drift in newer cmakes (C++20 module dyndep, custom
  command emission); fixes for whatever the matrix surfaces land
  here as they come up. Once the matrix has been green for a
  release cycle we can promote it to a blocking gate.
- **CI baseline.** A handful of e2e jobs (`cmake + bwrap`,
  `bazel build downstream`) fail intermittently for
  environment reasons (cmake-config bundle staging on the CI
  runner; userns / fuse permissions on Ubuntu 24.04 runners;
  bazel 9 toolchain expectations). These don't reflect
  product issues but they make PR review noisier than it
  should be. The previously-listed `hello-fuse pipeline` and
  `cas-fuse against fake CAS` jobs were retired alongside
  `cmd/cas-fuse` itself; bb_clientd is the production
  CAS-aware mount story now.

## Next

- **Wire the narrowing-undercoverage audit into a CI gate.**
  All the plumbing is in:
  - cmake oracle via convert-element's
    `--out-cmake-configure-reads`.
  - trace oracle via build-tracer's `--source-root` flag +
    `tracenorm.ExtractReads`.
  - per-element pattern surface emitted by write-a as
    `srckey-patterns.txt`.
  - `cmd/audit-narrowing` consumes patterns + oracle(s) and
    emits a sorted undercoverage report.
  - configure_file lift (`--cmake-configure-file-bin` on
    write-a) makes `*.h.in` safe to mark
    `exclude **/*.h.in` in patterns for elements whose
    templates lifted: with the full cmake-variable namespace
    captured into the values JSON
    (`cmakerun.filterVolatilePaths`), template edits that add
    `@VAR@` markers resolve correctly through the Bazel-time
    tool without convert-element rerunning. The
    `cmake-codegen-lifted` tag distinguishes lifted vs legacy
    genrules; legacy ones still have `.h.in` content-load-bearing
    and shouldn't be excluded.
  
  Missing piece: actually run the audit somewhere. Two
  conversations to have before flipping the gate on:
  - **Policy**: hard-fail on any drift, or accept a per-element
    allowlist of expected misses? Templates that didn't lift
    (the v1 Extract can't recover values from every shape)
    keep flagging their `.h.in` correctly. A whitelist mechanism
    plus an "expected drift" file alongside
    `srckey-patterns.txt` is the realistic landing.
  - **Per-element opt-in to capture** for trace-driven kinds:
    pass `--source-root` into the round-2 install genrule's
    build-tracer + trace-publish invocations (today
    `pipelineTracePublishStep` doesn't). Without this the
    trace oracle is empty and only the cmake side carries
    signal.
- **Same lift shape for `file(GENERATE)` and cmake-builtin
  `add_custom_command`s.** Wherever `lower/` currently reads
  bytes from the build dir to embed in a genrule's `cmd`, the
  same cache-key issue applies. The configure_file lift's
  pattern (template + values + Bazel-time tool) is reusable
  for these other configure-time codegen surfaces; sweep
  through `lower/codegen.go` and `lower/configure_file.go`'s
  callers, classify each by what cmake feature they recover,
  and lift the cleanly-tractable cases.
- **kind:cmake round-2 fallback for unliftable
  `execute_process`.** Phase B follow-on to the Now-bullet
  native lift. When `convert-element` exits with
  `unsupported-execute-process`, the round-2-style coarse
  "cmake configure + ninja + install" genrule takes over for
  that element — same destination as kind:autotools / make /
  perl / manual / script, but reached differently. kind:cmake
  is **not** a `pipelineHandler` variant (no
  `traceDrivenSrckeyPatterns` field to flip; no
  `shouldUseRound2()` branch), and it doesn't have an
  autotools-style `round2Enabled` build-wide flag either —
  flipping either would force every kind:cmake element through
  round-2, sacrificing the fine-grained native render. Instead
  the dispatch is **per-element**: `cmakeHandler` keeps the
  native render as the primary path, and the round-2 install
  genrule + placeholder BUILD.bazel.out are extra wiring
  emitted alongside, activated only when convert-element
  refuses the call. Reuses `cmd/build-tracer`,
  `@trace_<elem>//:trace`, and the inline `trace-publish`
  rendezvous machinery — all of which are kind-agnostic
  already. Render gate: `meta-cmake-round2.sh`. Live-AC gate:
  the publish/lookup wire half of
  `tools/e2e-meta-autotools-round2-live.sh` is kind-agnostic
  but its bazel-build half is autotools-fixture-specific; a
  cmake sibling gate is the v1 plan. Architectural recipe:
  `docs/design/cmake-execute-process-round2-fallback.md`.
- **Repo-rule install for kind:cmake round-2 fallback.**
  Phase B's round-2 fallback (per
  `docs/design/cmake-execute-process-round2-fallback.md`)
  transports the install tree as `install_tree.tar` between
  project B and project A's `BUILD.bazel.out`, costing roughly
  2× bytes in CAS (tar blob + extracted files via the
  in-`BUILD.bazel.out` extract genrule) and one extra Bazel
  action per consumer. Storage duplication adds up across a
  fleet. Alternative: a Bazel repository rule whose
  `repository_ctx.execute()` either runs cmake at loading
  time directly OR untars `install_tree.tar` into a
  per-element repo, exposing per-target labels without the
  extract genrule + CAS duplication. Precedent:
  `rules/traces.bzl`'s `_trace_repo` (loading-time AC
  lookup) — but that one only does AC `GetActionResult`, not
  a full build. Trade-offs: loading-time work blocks Bazel
  startup; repo rules don't run on RBE (executor-pool
  advantages forfeited); hermeticity weaker (relies on
  host-side cmake/ninja). Worth re-evaluating once fixtures
  reveal the storage-duplication cost in practice.
- **kind:meson** native render. Meson exposes
  `meson introspect --targets`, which is closer to cmake's File API
  than autotools' "you have to actually run it" introspection. The
  native render path probably looks like a kind:cmake variant rather
  than a kind:autotools variant.
- **`bst` wrapper** so `bst build` works against a converted project
  (and against `bst workspace open`-modified element source trees).
  Goal: BuildStream developers' muscle memory keeps working through
  the transition.

## Later (research / open questions)

- **Source-side AC narrowing for autotools.** Bazel's hermetic-action
  model says inputs in → outputs out; you can't have a byte be
  available to the action at exec time without it being in the AC
  key. So narrowing autotools is unavoidably a side-channel story.
  `docs/three-pass-flow.md` lays out three options (FUSE, host-fs
  source cache via `--repo_env`, write-a-time registry) and rules
  out two; the third is the path forward but the value-vs-complexity
  trade-off is open.
- **kind coverage breadth.** `kind:script` / `kind:pyproject` /
  `kind:flatpak_image` / `kind:snap_image` / `kind:collect_*` all
  have placeholder handlers today. Each kind is bounded work; what's
  not bounded is the question of which kinds are graph-recoverable
  vs need-to-stay-coarse.
- **Drop the host-toolchain assumption from CI / e2e gates.**
  Several gates expect cmake / ninja / bwrap / fuse3 installed on
  the host machine (CI runner or developer workstation). In a
  full remote-execution setup these belong on the executor, not
  on the dev's box — the dev only needs Bazel + bb_clientd. Walk
  the gate scripts and the `make check-tools` surface, identify
  which host-tool dependencies are CI-runner artifacts (vs
  hard build-tool needs we can't push remote), and migrate the
  former onto the executor toolchain.

## Done (high points)

- **Configure_file lift.** Per-element `*.h.in` templates are
  no longer load-bearing inputs of convert-element's cache key
  for elements whose templates lift. Convert-element captures
  the FULL cmake variable namespace at end-of-configure
  (`cmakerun/dump-vars.cmake` registers a deferred callback
  that dumps every variable; `cmakerun.filterVolatilePaths`
  drops path-bearing vars so the dump is byte-stable across
  cmake invocations). The recovered genrule emits with the
  .h.in as a real Bazel `srcs` input plus a
  `//tools:cmake-configure-file` invocation that re-runs
  cmake's substitution at Bazel build time. Edits to .h.in
  — including ones that introduce new `@VAR@` markers —
  invalidate the genrule directly through Bazel's source graph
  and resolve correctly via the namespace dump; convert-
  element doesn't have to rerun. Opt-in via `write-a
  --cmake-configure-file-bin=<path>` (the binary gets staged
  into both projects' `tools/` and the per-element genrule
  passes `--lift-configure-file=true` to convert-element).
  Templates the verify-pass can't reproduce (Substitute hasn't
  modeled an option, or the template references a filtered
  volatile variable) fall back to the legacy base64-cmd shape;
  for those, .h.in stays content-load-bearing in srckey. The
  `cmake-codegen-lifted` tag distinguishes lifted vs legacy
  genrules at query time. Recipe:
  `docs/design/narrowing-audit.md`.
- **Narrowing-undercoverage audit.** cmake oracle from
  build.ninja's `RERUN_CMAKE` deps + trace oracle from
  build-tracer's openat capture (opt-in via `--source-root`)
  + per-element patterns surface emitted by write-a +
  `cmd/audit-narrowing` consumer that diffs the two and
  emits an undercoverage report. Recipe:
  `docs/design/narrowing-audit.md`.
- Two-pass meta-project shape: `cmd/write-a` renders project A and
  project B, Bazel owns cross-element scheduling and caching.
- `kind:cmake` native render: cmake's File API + `--trace-expand`
  drive convert-element to emit `cc_library` / `cc_binary` rules.
  Zero-stub narrowing means `.c`-only edits cache-hit at the
  convert action.
- `kind:autotools` round-1 native render: build-tracer wraps
  `configure && make && make install`; the trace + `make -np`
  feed `convert-element-autotools`; install genrule lives in
  project B with deps as proper Bazel targets.
- `kind:autotools` round-2 graph derivation. Project A's
  per-element converter genrule consumes `@trace_<elem>//:trace`,
  a load-time `_trace_repo` lookup against the REAPI
  ActionCache keyed by `SyntheticActionDigest(srckey)`. Project
  B's install genrule ends with an inline `trace-publish` call
  that lands the AC entry. Round-2 is the default; pass
  `--autotools-round1` to opt back into the legacy single-
  genrule shape. Render-half gate: `meta-autotools-round2.sh`.
  Live-AC gate (buildbarn + optionally bb_clientd):
  `tools/e2e-meta-autotools-round2-live.sh`. Recipe:
  `docs/design/autotools-round2-rendezvous.md`.
- **kind:make joins the round-2 trace-driven path.** Same
  architecture as kind:autotools — opt-in via the
  `traceDrivenSrckeyPatterns` field on the kind's
  `pipelineHandler` (`handler_make.go:makeSrckeyPatterns`).
  When the trace-driven binaries are supplied to write-a, kind:make
  elements render with the converter genrule in project A and the
  build-tracer-wrapped install genrule + inline trace-publish in
  project B. Render-half gate: `meta-make-round2.sh`. The
  kind-agnostic live-AC gate
  (`tools/e2e-meta-autotools-round2-live.sh`) covers the
  bazel-build-half wire contract end-to-end against bb_clientd,
  applying to any opted-in kind.
- **kind:makemaker, kind:modulebuild, kind:manual, kind:script
  join the trace-driven path.** Same one-line opt-in pattern
  kind:make established. Per-kind srckey narrowing:
  kind:makemaker tracks Makefile.PL + *.xs + *.h family;
  kind:modulebuild tracks Build.PL + *.xs + *.h family;
  kind:manual + kind:script use the conservative
  content-everything default (no kind-level signal for which
  files drive build commands — per-element narrowing comes
  via the existing read-paths.txt sibling). Coverage:
  `cmd/write-a/handler_pipeline_round2_test.go` table-driven
  test asserts the round-2 shape end-to-end for each kind.
- **Bazel 9 CAS-aware filesystem.** Bazel 9 dropped
  `--unix_digest_hash_attribute_name` (the xattr fast-path that
  let cas-fuse tell Bazel "trust this pre-computed digest, don't
  re-hash") without a direct replacement. Replacement: adopt
  `bb_clientd` as a Bazel-9 companion daemon — paired with Bazel
  via the surviving `--experimental_remote_output_service=` flag,
  serving a FUSE mount + speaking RemoteOutputService so Bazel
  trusts daemon-reported digests. **Not** an adoption of buildbarn
  end-to-end; bb_clientd talks plain REAPI to whatever CAS endpoint
  it's pointed at (the same way `bazelisk` pairs with `bazel`).
  Output-side BwoB (lazy materialisation of build artifacts) is a
  free side effect. Wiring: `make bb-clientd-up`/`down`,
  `deploy/buildbarn/config/bb_clientd.jsonnet`. Local end-to-end
  exercise: `tools/e2e-hello-bbclientd.sh` (also `make
  e2e-hello-bbclientd`); not yet wired into GitHub Actions CI
  because the runners don't ship bb_clientd by default — adding
  it as a CI step would self-skip until that changes.
  `rules/sources.bzl` + `rules/traces.bzl` parameterise the
  mount-side path layout via `CAS_DIRECTORY_PREFIX` (default
  `blobs` historically — the cmd/cas-fuse layout; bb_clientd
  users pass `cas/<instance>/blobs/<digest_function>`).
  `cmd/cas-fuse` and `internal/casfuse` were retired in a
  follow-up after bb_clientd proved itself the production
  path. Recipe: `docs/design/bazel9-cas-fs.md`.
- Trace + make-db canonicalization (pids stripped, gcc temp paths
  placeholdered, action-time mktemp paths normalized). Foundation
  for round-2 cache reuse.
- Per-element srckey + per-kind narrowing patterns — defines what
  counts as graph-affecting vs name-only for the autotools build.

The "Done" list is in the rear-view; the doc that captures the
current state of the codebase is `docs/architecture.md` (top-down)
plus `docs/build-structure.md` (interop contract) plus
`docs/three-pass-flow.md` (build-time flow).
