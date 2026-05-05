# buildstream-bazel

**Take your BuildStream project to Bazel — for real, not a wrapper.**

If you maintain a [BuildStream](https://www.buildstream.build/)
project — say, the
[FreeDesktop SDK](https://gitlab.com/freedesktop-sdk/freedesktop-sdk)
or anything close to its shape — and you've ever wanted Bazel's
remote cache, hermetic actions, IDE integration, and native
incremental builds without giving up the BuildStream graph you've
already modeled, this is the tool that gets you there.

It converts a `.bst` element graph into a Bazel workspace where every
element compiles through real `cc_library` / `cc_binary` rules. No
`bst` daemon at build time. No opaque "wrap the whole upstream build
in one giant genrule." Per-translation-unit incremental rebuilds.
Cache hits across machines through standard Bazel remote cache.

This is a **transition tool**. The success state is "you don't need
buildstream-bazel anymore — your downstream is plain Bazel." Run it
once on a project, commit the generated BUILD files, ship.

## What you actually get

- **Native cc rules.** A `kind:cmake` element becomes the same
  `cc_library`s you'd write by hand. A `kind:autotools` element
  goes through a one-time trace-driven build that recovers the
  same set. No runtime indirection — `bazel build //...` over the
  output is plain Bazel.
- **Wide kind coverage.** `cmake`, `autotools`, `make`, `manual`,
  `script`, `stack`, `filter`, `compose`, `import`, `pyproject`,
  `flatpak_image`, `snap_image`, plus arch / option conditional
  dispatch lowered to Bazel `select()`. The dominant 67% of FDSDK
  ships through the high-fidelity converters; the long tail goes
  through coarse install-tree pipelines that still produce real
  Bazel targets downstream consumers can depend on.
- **Faithful cross-element wiring.** `find_package` resolves to
  proper Bazel `deps`. `pkg_check_modules` ditto. Library link
  flags map to `cc_library` deps. None of this is heuristic; the
  cmake side reads cmake's File API codemodel +
  `--trace-expand`, and the autotools side reads an actual
  process trace.
- **Hermetic and reproducible.** Every render is deterministic.
  Trace canonicalization (pid stripping, gcc temp-path
  placeholding, action-time mktemp neutralization) makes the
  autotools build's output byte-stable across machines, so your
  remote cache actually hits.

## How it works in 30 seconds

```mermaid
flowchart LR
  bst[".bst graph"] --> wa[write-a]
  wa --> A["project A<br/>(meta workspace)"]
  A --> bA[bazel build A]
  bA --> B["project B<br/>(real Bazel workspace)"]
  B --> bB[bazel build B]
  bB --> art[binaries / libraries]
```

Two passes. **Project A** is a meta workspace whose only purpose
is converting elements. Each element gets a `genrule` that invokes
the per-kind translator. **Project B** is the materialized output
— a normal Bazel workspace with `cc_library` / `cc_binary` rules
your team builds against.

The architectural deep-dive lives in
[`docs/overview.md`](docs/overview.md) (5-minute read with
flowcharts) and [`docs/three-pass-flow.md`](docs/three-pass-flow.md)
(the build-time pass model).

## Quick start

```sh
# 1. Build the converter binaries.
make all

# 2. Smoke test against the hello-world fixture.
make e2e-meta-hello

# 3. Drive the trace-driven autotools path end-to-end.
make e2e-meta-autotools-native

# 4. Convert your own BuildStream project.
build/bin/write-a \
    --bst path/to/yours.bst \
    --out /tmp/project-a \
    --out-b /tmp/project-b \
    --convert-element           build/bin/convert-element \
    --convert-element-autotools build/bin/convert-element-autotools \
    --build-tracer-bin          build/bin/build-tracer
```

Then `cd /tmp/project-b && bazel build //...`. That's it.

You'll need Bazel ≥ 9 (bzlmod + RemoteOutputService) to
consume the output with the production CAS-aware mount path
(bb_clientd serves source bytes; Bazel trusts the daemon's
digests without re-hashing). Bazel 7 / 8 still build the
generated workspaces — the CAS-mount path falls back to
`cmd/cas-fuse` with the `--unix_digest_hash_attribute_name`
xattr fast-path Bazel ≤ 8 carried — but that's the legacy
shape; the project's main story is Bazel 9 + bb_clientd. See
[`docs/design/bazel9-cas-fs.md`](docs/design/bazel9-cas-fs.md)
for the rationale.
Cmake-side conversion needs `cmake` and `bwrap` on the host;
autotools-side needs `cmake`, `make`, and either Linux/amd64
(native ptrace) or `strace` on `$PATH`. See
[`docs/architecture.md`](docs/architecture.md) for the full
host-tool table.

## Trying the FreeDesktop SDK

FDSDK is the working target — 1,092 elements across 21 kinds.
It's the corpus that drives every fidelity decision in this
repo. If you want to see what conversion of a real, demanding
BuildStream project looks like:

```sh
make fdsdk-reality-check     # surveys the FDSDK graph; stops at first kind we don't handle
```

Empirical coverage status lives in
[`docs/fdsdk-coverage-status.md`](docs/fdsdk-coverage-status.md);
known-delta catalog in
[`docs/cmake-conversion-deltas.md`](docs/cmake-conversion-deltas.md).
Both are honest about what's not yet handled — this is a tool
under active development against a real-world project, not a
clean-room implementation.

## Repository layout

| Path | What's there |
|---|---|
| `cmd/write-a/` | Renders project A + project B from a `.bst` graph. The thing you actually run. |
| `cmd/build-tracer/` | Process tracer for the autotools native path (native ptrace + strace fallback, canonical output). |
| `cmd/convert-element-autotools/` | Trace + `make -np` → native cc rules. |
| `converter/` | The cmake converter. cmake File API codemodel + `--trace-expand` → native cc rules. |
| `orchestrator/` | Predecessor single-project orchestrator. Kept for the regression-diff machinery; new kinds land in `cmd/write-a/`. |
| `internal/` | Shared packages — CAS, FUSE, manifest, shadow tree, fidelity. |
| `testdata/meta-project/` | End-to-end fixtures driven by the gates under `scripts/`. |
| `docs/` | Architecture references; see [`ROADMAP.md`](ROADMAP.md) for what's done / next. |

## Where to go next

- **Want to run the converter?** Quick-start above, then
  [`docs/architecture.md`](docs/architecture.md) for the
  host-tool table and CLI reference.
- **Want to understand the design?**
  [`docs/overview.md`](docs/overview.md) →
  [`docs/three-pass-flow.md`](docs/three-pass-flow.md) →
  [`docs/build-structure.md`](docs/build-structure.md).
- **Want to contribute?** [`CONTRIBUTING.md`](CONTRIBUTING.md)
  has the dev-loop commands and the per-handler test map.
- **Curious what's next?** [`ROADMAP.md`](ROADMAP.md).

## License

Apache 2.0 — see [`LICENSE`](LICENSE) for the full text and
[`NOTICE`](NOTICE) for attribution.
