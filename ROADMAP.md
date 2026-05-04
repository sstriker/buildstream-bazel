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
- **CI baseline.** A handful of e2e jobs (`cmake + bwrap`,
  `bazel build downstream`, `hello-fuse pipeline`) fail on main
  for environment reasons (cmake-config bundle staging on the CI
  runner; bazel 9 dropped `--unix_digest_hash_attribute_name`).
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
