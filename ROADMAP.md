# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

- **`kind:autotools` round-2 graph derivation.** Round 1 (the
  trace-driven coarse genrule in project B, the canonical trace
  output, the per-element srckey) ships today. Round 2 — write-a
  consults a srckey → registered-trace lookup at render time and
  emits fine-grained `cc_library` / `cc_binary` into project B
  directly when the registry hits — is the next concrete piece. See
  `docs/three-pass-flow.md` for the architectural arc and the
  precise contract round-2 needs to honor.
- **Bazel 9 CAS-aware filesystem.** Bazel 9 dropped
  `--unix_digest_hash_attribute_name` — the flag that let the
  cas-fuse FUSE mount tell Bazel "trust this pre-computed
  digest, don't re-hash" — without a direct replacement. The
  mount still resolves correctly on Bazel 9 and the BwoB
  properties (executor-side reads from CAS, no source-tree
  materialisation on dev disk) are preserved, but Bazel
  re-hashes every input the FUSE daemon already knows the
  digest of. First builds of fresh checkouts pay the full
  O(source-bytes) re-read + hash cost.
  **Direction picked** (see `docs/bazel9-cas-fs.md` for the
  full analysis): adopt **`bb_clientd`** as a Bazel 9
  companion daemon, paired with Bazel via the surviving
  `--remote_output_service=` flag. bb_clientd serves a
  FUSE/NFS mount and reports digests over the
  `RemoteOutputService` protocol; Bazel trusts those digests
  without re-hashing. This is *not* an adoption of buildbarn
  end-to-end — bb_clientd talks plain REAPI to whatever CAS
  endpoint we point it at, the same way `bazelisk` talks to
  whatever Bazel binary it pins. Output-side BwoB (lazy
  materialisation of build artifacts) lands as a free side
  effect.
- **CI baseline.** A handful of e2e jobs (`cmake + bwrap`,
  `bazel build downstream`, `hello-fuse pipeline`,
  `cas-fuse against fake CAS`) fail intermittently or
  consistently for environment reasons (cmake-config bundle
  staging on the CI runner; userns / fuse permissions on
  Ubuntu 24.04 runners; bazel 9 toolchain expectations).
  These don't reflect product issues but they make PR review
  noisier than it should be.

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
- Trace + make-db canonicalization (pids stripped, gcc temp paths
  placeholdered, action-time mktemp paths normalized). Foundation
  for round-2 cache reuse.
- Per-element srckey + per-kind narrowing patterns — defines what
  counts as graph-affecting vs name-only for the autotools build.

The "Done" list is in the rear-view; the doc that captures the
current state of the codebase is `docs/architecture.md` (top-down)
plus `docs/build-structure.md` (interop contract) plus
`docs/three-pass-flow.md` (build-time flow).
