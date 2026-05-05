# Contributing — the verify/test loop

This doc captures the dev-loop commands that confirm a code
change is correct. Aimed at humans and AI agents alike. If
you're an agent, read this end-to-end before making changes;
the commands here are the contract for "is this change ready".

## What to run, when

Decide based on what you touched:

| You changed | Run |
|---|---|
| Anything `*.go` | `go build ./...` then `go vet ./...` then `gofmt -l .` (must be empty) then `go test ./...` |
| `cmd/write-a/handler_*.go` (any handler) | the relevant `scripts/meta-*.sh` render gate (see [render gates](#render-gates)) |
| `cmd/build-tracer/` | `go test ./cmd/build-tracer/...` plus a render gate that exercises the autotools native path (`scripts/meta-autotools-native.sh`) |
| `cmd/convert-element-autotools/` | `go test ./cmd/convert-element-autotools/...` plus the autotools render gates |
| `converter/...` (cmake-side) | `go test ./converter/...` (unit), `make e2e-orchestrate` (needs cmake + bwrap) |
| `Makefile` / `scripts/` | the script(s) you touched, plus run their `make e2e-meta-*` target if it exists |
| Anything in `docs/` | nothing. CI's docs jobs render via GitHub markdown. |

## The full pre-commit checklist

Before committing or pushing:

```sh
# 1. Code compiles + lint clean.
go build ./...
go vet ./...
gofmt -l .                 # must print nothing

# 2. Unit tests pass.
go test ./...

# 3. Render gates relevant to your change pass.
scripts/meta-autotools-native.sh
scripts/meta-autotools-multitarget.sh
# ... see Render gates below for the full list
```

If any of step 1–3 prints output other than the green-path
expected lines, the change isn't ready.

## Render gates

Render gates live under `scripts/` as `meta-*.sh`. Each
exercises one or more `cmd/write-a` handler paths against a
fixture under `testdata/meta-project/`. They're shell scripts
because they shell out to `write-a`, `bazel`, and bazel-built
artifacts; Go test harness wouldn't add anything.

Bazel build inside a render gate is gated on `bazel >= 7` being
on `$PATH`. When bazel is unavailable, the gate runs the
**render** half (write-a output) and skips the bazel-build half
with a `skipping build phase` message. **That partial run is
still meaningful** — the rendered shape is the contract write-a
owes its consumers.

Mapping handler → gate:

| Handler | Gate(s) |
|---|---|
| `handler_cmake.go` | `scripts/meta-hello.sh`, `scripts/meta-stack.sh`, `scripts/meta-cross-cmake.sh` |
| `handler_autotools_native.go` | `scripts/meta-autotools-native.sh`, `scripts/meta-autotools-multitarget.sh`, `scripts/meta-autotools-tu-optflags.sh`, `scripts/meta-autotools-libtool-pic.sh` |
| `handler_autotools.go` (coarse) | `scripts/meta-autotools.sh` |
| `handler_manual.go` | `scripts/meta-manual.sh`, `scripts/meta-vars.sh` |
| `handler_make.go` | `scripts/meta-make.sh` |
| `handler_compose.go` | `scripts/meta-compose.sh` |
| `handler_filter.go` | `scripts/meta-filter.sh` |
| `handler_import.go` | `scripts/meta-import.sh` |
| `handler_collect_manifest.go` | `scripts/meta-collect-manifest.sh` (if present) |
| `handler_script.go` | `scripts/meta-script.sh` |
| `handler_bazel.go` (passthrough) | `scripts/meta-bazel-passthrough.sh` |

`make` knows these too — `make e2e-meta-autotools-native`
for example. `make help` lists all targets.

## Investigating a failure

Common failure modes and how to diagnose:

- **`gofmt -l` prints a path**: run `gofmt -w <path>`. The
  comment-alignment tab heuristic is a frequent source of
  drift — when adding a new entry to a slice of struct
  literals with line-end comments, the longest line drives
  every other line's column.
- **A render gate prints `missing marker: <X>`**: a test
  expectation in the script. Either the rendered output
  legitimately changed (update the gate) or you broke the
  rendering (fix the handler). The script `cat`s the
  rendered BUILD.bazel above the failure for diff inspection.
- **A unit test fails with a missing/extra marker**: same
  shape as above but for `cmd/write-a/main_test.go`'s
  `TestWriter_*` suite. The body of the rendered file is
  printed below the assertion.
- **`go vet ./...` prints `... not used`**: an import or
  variable became dead after a refactor. Remove or use.
- **Build failure with `undefined: <symbol>`**: the symbol
  lives behind a build tag (e.g., `//go:build linux,amd64`)
  or a different package — check the import.

## Touchpoints by topic

- **Rendering project A or B**: `cmd/write-a/main.go`'s
  `writeProjectA` / `writeProjectB`. Per-element handlers
  in `cmd/write-a/handler_*.go`.
- **The build trace** (autotools): `cmd/build-tracer/`.
- **The autotools converter**: `cmd/convert-element-autotools/`.
- **The cmake converter**: `converter/cmd/convert-element` +
  `converter/internal/`.
- **Source-key + content-narrowing patterns**:
  `cmd/write-a/srckey.go` + per-handler
  `<kind>SrckeyPatterns()`.
- **Project-level architecture story**:
  `docs/three-pass-flow.md`, `docs/overview.md`.

## What to skip

These do NOT need to run for most changes:

- `make e2e-buildbarn` / `make e2e-buildbarn-execute` — needs
  Docker + Buildbarn substrate. CI runs them; local runs are
  rarely necessary.
- `make e2e-fmt` — re-clones the `fmt` library and converts
  it. Slow. Only run when touching the cmake conversion
  fidelity story.
- `make fdsdk-reality-check` — exploratory probe over the
  FreeDesktop SDK graph. Only run when expanding kind
  coverage.

## Commit + PR conventions

See `CLAUDE.md` for commit-message and PR-description rules
(no `claude.ai/code/session_*` URLs, etc.). Reading short:

- Commit messages explain the *why*, not the *what*. Diff
  shows the what.
- PRs against `main`. Stack-of-PRs is fine — note the base
  branch in the description.
- Don't include auto-generated session URLs anywhere.
