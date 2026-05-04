# cmake-to-bazel

Convert a [BuildStream](https://www.buildstream.build/) project — the
[FreeDesktop SDK](https://gitlab.com/freedesktop-sdk/freedesktop-sdk)
is the working target — into a Bazel workspace that builds the same
artifacts.

## What you get

- A pair of generated Bazel workspaces: **project A** (the meta
  workspace whose genrules invoke per-kind translators) and **project
  B** (the consumer workspace built against project A's outputs).
  `bazel build //...` over project B compiles the FDSDK with no
  BuildStream / `bst` runtime dep.
- Native `cc_library` / `cc_binary` rules — not a single opaque
  install genrule. Per-element conversion goes deep enough that
  Bazel's incremental build and remote cache handle the FDSDK as a
  first-class consumer.
- Full faithful conversion of `kind:cmake`, `kind:autotools`,
  `kind:make`, `kind:manual`, `kind:script`, `kind:stack`,
  `kind:filter`, `kind:compose`, `kind:import`, plus arch / option
  conditional dispatch via Bazel `select()`.

See [docs/overview.md](docs/overview.md) for the architecture in five
minutes (with flowcharts), or jump to:

- [docs/whole-project-plan.md](docs/whole-project-plan.md) — the
  whole-project-plan: phases, scope, what's done.
- [docs/architecture.md](docs/architecture.md) — what's actually in
  this repo today.
- [docs/trace-driven-autotools.md](docs/trace-driven-autotools.md) —
  the trace-driven autotools converter.
- [docs/cmake-conversion-deltas.md](docs/cmake-conversion-deltas.md)
  — known correctness gaps in the `kind:cmake` converter.

## Quick start

```sh
# 1. Build the binaries.
make converter

# 2. Run the hello-world end-to-end gate.
make e2e-meta-hello

# 3. Run the trace-driven autotools end-to-end gate.
make e2e-meta-autotools-native

# 4. Run every meta-project gate.
make e2e
```

End-to-end gates use Bazel ≥ 7 (bzlmod). Sandboxed environments
without `bcr.bazel.build` egress can override the registry via
`META_BAZEL_BUILD_ARGS=--registry=<mirror>`; see
[`scripts/meta-hello.sh`](scripts/meta-hello.sh) for the full
override pattern.

## Repository layout

| Path | What's there |
|---|---|
| `cmd/write-a/` | Renders project A + project B from a `.bst` graph. |
| `cmd/build-tracer/` | In-action process tracer (native ptrace, strace fallback). |
| `cmd/convert-element-autotools/` | Trace → native cc rules for `kind:autotools`. |
| `cmd/convert-element-autotools/` | Trace → native cc rules for `kind:autotools`. |
| `converter/` | The cmake converter (cmake → cc rules). |
| `orchestrator/` | The legacy single-project orchestrator (M1-M3). |
| `internal/` | Shared packages (manifest, shadow, cas, fidelity, ...). |
| `testdata/meta-project/` | End-to-end fixtures driven by `scripts/meta-*.sh`. |
| `docs/` | Plans, architecture, conversion-delta docs. |

## Licensing

Apache License, Version 2.0. See [LICENSE](LICENSE) for the full
text and [NOTICE](NOTICE) for attribution.
