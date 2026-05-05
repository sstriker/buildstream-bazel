# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

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
  applying to any opted-in kind. Adding more pipeline kinds
  (kind:makemaker / modulebuild / manual / script) is now a
  one-line opt-in in their `init()`.
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
