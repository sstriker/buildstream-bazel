# buildstream-bazel

**Take your BuildStream project to Bazel — for real, not a wrapper.**

> ⚠️ **Proof of concept.** This repository is exploratory work, not a
> product. There are no releases — clone and build from source. Your
> mileage may vary: it may or may not succeed on your project. Expect
> rough edges.

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
- **Wide kind coverage.** Native, trace-driven converters for
  `cmake`, `autotools`, `make`, `makemaker`, `modulebuild`,
  `manual`, and `script`. Filegroup-composition kinds (`stack`,
  `filter`, `compose`, `import`) lower to Starlark over other
  elements' outputs, no per-kind translator runs. `kind:bazel`
  is a passthrough — the element's source tree already carries
  BUILD files, which write-a stages verbatim into project B
  (useful for upstream Bazel-native sources or for hand-edited
  forks of converter output). Coarse placeholder handlers for
  `pyproject`, `flatpak_image`, `snap_image` produce
  dependency-correct Bazel targets but don't yet reconstruct
  the kind's output bytes — see `ROADMAP.md` "Later" for the
  trade-off. Plus arch / option conditional dispatch lowered
  to Bazel `select()`. The dominant 67% of FDSDK ships through
  the high-fidelity converters; the long tail goes through
  coarse install-tree pipelines that still produce real Bazel
  targets downstream consumers can depend on.
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

The architecture story lives in
[`docs/architecture.md`](docs/architecture.md) (single doc, prose +
diagrams, the two workspaces + three passes + per-kind paths +
cost model). For deeper design specs of individual mechanisms see
[`docs/design/`](docs/design/).

## Quick start

```sh
# 1. Build the converter binaries.
make all

# 2. Smoke test against the hello-world fixture.
make e2e-meta-hello

# 3. Drive the trace-driven autotools path end-to-end.
make e2e-meta-autotools-native

# 4. Convert your own BuildStream project. write-a takes four
#    operator-facing dials:
#
#      --fidelity={strict|best-effort}  refusal handling (default strict)
#      --bake-in={warn|allow|reject}    convert-time-bake policy (default warn)
#      --diagnostics                    collect every Tier-1 refusal instead
#                                       of aborting on the first
#      --deployment={auto|local|production}  trace-driven kinds' install shape
#                                            (default auto: production if the
#                                            REAPI AC publish+lookup binaries
#                                            are wired, else local)
#
#    --fidelity / --bake-in / --diagnostics are threaded verbatim
#    into every converter's matching --fidelity / --bake-in /
#    --diagnostics flag; each converter decides what they mean for
#    its own kind. Per-kind escape hatches (--cmake-round2-fallback,
#    --meson-round2-fallback, --pyproject-fallback,
#    --unsupported-execute-process-fallback, ...) still work and
#    override the dial-derived defaults.
build/bin/write-a \
    --bst path/to/yours.bst \
    --out /tmp/project-a \
    --out-b /tmp/project-b \
    --convert-element-cmake     build/bin/convert-element-cmake \
    --convert-element-trace     build/bin/convert-element-trace \
    --build-tracer-bin          build/bin/build-tracer \
    --deployment=local
```

Then `cd /tmp/project-b && bazel build //...`. That's it.

### `bst`-style wrapper (optional)

If your team is more comfortable with BuildStream's CLI than with
Bazel's, `tools/bst` is a thin wrapper that forwards `bst build`,
`bst show`, and `bst workspace open|close` to the equivalent
write-a + bazel-build sequence. Same end state — project A + B
under a per-element cache dir — without re-learning the Bazel CLI
upfront:

```sh
tools/bst build path/to/yours.bst
tools/bst workspace open path/to/yours.bst /tmp/scratch   # edit sources
tools/bst build path/to/yours.bst                         # picks up edits
tools/bst workspace close path/to/yours.bst               # restore
```

Once teams are comfortable invoking `bazel build` against project
B directly, the wrapper becomes optional. The render half is
covered by `make e2e-meta-bst-wrapper`.

You'll need Bazel ≥ 7 (bzlmod) to consume the output — project
B is a normal Bazel workspace with `cc_library` / `cc_binary`
rules and no CAS-mount machinery, so any Bazel that supports
bzlmod builds it.

To turn on the optional CAS-aware source mount (sources stream
into action sandboxes through a daemon-served FUSE mount, no
materialisation on dev disk), use Bazel 9 + `bb_clientd`. See
[`docs/design/sources.md`](docs/design/sources.md).

Cmake-side conversion needs `cmake` on the host; autotools-side
needs `cmake`, `make`, and either Linux/amd64 (native ptrace) or
`strace` on `$PATH`. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for
the full host-tool table.

## Trying the FreeDesktop SDK

FDSDK is the working target — 1,092 elements across 21 kinds.
It's the corpus that drives every fidelity decision in this
repo. If you want to see what conversion of a real, demanding
BuildStream project looks like:

```sh
make fdsdk-reality-check     # surveys the FDSDK graph; stops at first kind we don't handle
```

Empirical coverage status lives in
[`docs/fdsdk-coverage.md`](docs/fdsdk-coverage.md); known-delta
catalogues in [`docs/cmake-conversion-deltas.md`](docs/cmake-conversion-deltas.md)
(cmake → converter) and
[`docs/fidelity-deltas.md`](docs/fidelity-deltas.md) (cmake-built vs
Bazel-built artifact diffs). Both are
honest about what's not yet handled — this is a tool under active
development against a real-world project, not a clean-room
implementation.

## Repository layout

| Path | What's there |
|---|---|
| `cmd/write-a/` | Renders project A + project B from a `.bst` graph. The thing you actually run. |
| `cmd/build-tracer/` | Process tracer for the autotools native path (native ptrace + strace fallback, canonical output). |
| `cmd/convert-element-trace/` | Trace + `make -np` → native cc rules. |
| `converter/` | The cmake converter. cmake File API codemodel + `--trace-expand` → native cc rules. |
| `internal/` | Shared packages — CAS, REAPI, manifest, shadow tree, fidelity, the `.bst` element parser, regression, sourcecheckout, exports, etc. See [`docs/codebase-map.md`](docs/codebase-map.md). |
| `testdata/meta-project/` | End-to-end fixtures driven by the gates under `scripts/`. |
| `docs/` | Architecture, codebase map, design specs. See [`ROADMAP.md`](ROADMAP.md) for what's planned. |

## Where to go next

- **Run the converter** — quick-start above, then
  [`docs/architecture.md`](docs/architecture.md) for the workspace
  shape and the per-kind paths.
- **Understand the design** —
  [`docs/architecture.md`](docs/architecture.md) for the story;
  [`docs/design/`](docs/design/) for the key mechanism specs
  (rendezvous, convergence loop, finalize-b, sources, narrowing
  audit).
- **See a conversion side by side** — a small CMake project next to
  its converted `BUILD.bazel` tree, directory for directory:
  [`docs/cmake-split-packages-example.md`](docs/cmake-split-packages-example.md).
- **Develop the converter** — [`docs/codebase-map.md`](docs/codebase-map.md)
  for the package tour; [`CONTRIBUTING.md`](CONTRIBUTING.md) for the
  dev-loop commands and per-handler test map.
- **See what's next** — [`ROADMAP.md`](ROADMAP.md).

## License

Apache 2.0 — see [`LICENSE`](LICENSE) for the full text and
[`NOTICE`](NOTICE) for attribution.
