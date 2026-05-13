# Meson native render — design

Architecture recipe for `converter/cmd/convert-element-meson` —
the Phase A introspection-driven kind:meson translator. Coverage
context: `docs/fdsdk-coverage-status.md` (meson is 12.3% of
FDSDK). Roadmap state: `ROADMAP.md` Done (Phase A) + Next
(install-plan-driven Phase B fallback).

This doc captures the v1 architecture, the patterns covered, and
the patterns refused.

## What meson exposes (and why this is tractable)

`meson setup` produces `<build>/meson-info/*.json`. After one
configure pass we can read:

- `intro-targets.json` — the build graph. Each target carries:
  - `name`, `id`, `type` (`static library`, `shared library`,
    `executable`, `custom`, `run`, `jar`).
  - `defined_in` (path to the `meson.build` that declared it),
    `subproject` (null for the top project).
  - `filename` (output path[s] in the build dir).
  - `target_sources` — mixed list. cc-shaped entries carry
    `language`, `machine`, `compiler`, `parameters` (full compile
    argv minus the input/output), `sources` (absolute paths),
    `generated_sources`. Linker entries carry `linker` +
    `parameters`.
  - `dependencies` — names of external dependencies (resolved via
    `dependency('foo')`).
  - `depends` — IDs of in-project targets this target depends on.
  - `installed` + `install_filename`.
- `intro-projectinfo.json` — project name, version.
- `intro-install_plan.json` — structured install destinations
  (`{libdir_static}`, `{bindir}`, …).
- `intro-dependencies.json` — resolved external deps with
  `compile_args` / `link_args`.
- `intro-buildoptions.json` — option values; the `section: user`
  rows mirror `meson_options.txt` / `meson.options`.
- `intro-buildsystem_files.json` — every `meson.build` /
  `meson_options.txt` / sub-include the configure pass read.
- `intro-compilers.json` — host/build compiler ID + version.

Critically, `intro-targets.json` is **per-target structured**:
sources, compile flags, dependencies, install paths are all
already separated. No need to parse `build.ninja` (cmake's
File API doesn't fully cover custom commands — meson's does).

## Pipeline

```
                   ┌────────────────────────┐
   source tree ──▶ │ meson setup <bd> <src> │ ──▶ <bd>/meson-info/*.json
                   └────────────────────────┘
                   (meson CLI is build-dir first, source-dir second)
                                │
                                ▼
                   ┌──────────────────────────┐
                   │ parse intro-* + projection│
                   │   (path normalization,    │
                   │    -I/-D/-other split)    │
                   └──────────────────────────┘
                                │
                                ▼
                   ┌────────────────────┐
                   │   ir.Package       │  (shared with kind:cmake)
                   │   ir.Target × N    │
                   └────────────────────┘
                                │
                                ▼
                   ┌─────────────────────────┐
                   │  emit/bazel.Emit        │  (shared)
                   └─────────────────────────┘
                                │
                                ▼
                       BUILD.bazel.out
```

The `converter/cmd/convert-element-meson/main.go` binary is a
sibling of `converter/cmd/convert-element-cmake/main.go` (the cmake
converter). Both consume an external build-system's introspection
and produce IR + BUILD.bazel.out via the same emit package.

## Per-target lowering rules

| Meson `type`       | IR `Kind`                | Notes                                    |
|--------------------|--------------------------|------------------------------------------|
| `static library`   | `KindCCLibrary` + `Linkstatic=true` | `ArtifactName` from `filename` basename. |
| `shared library`   | `KindCCLibrary`          | Defaults to dynamic linking.             |
| `both libraries`   | `KindCCLibrary`          | Single rule (Bazel decides static vs shared via toolchain). |
| `executable`       | `KindCCBinary`           | -                                        |
| `custom`           | `KindGenrule` (best-effort) | Lifted only when the command's argv contains exactly the standalone tokens `@INPUT@` / `@OUTPUT@` (no embedded or indexed forms) and no host-probing tools. Argv that lacks the expected token (or carries an unsupported substitution) refuses. |
| `run`              | silently skipped | Developer-convenience target (meson's analog of `add_custom_target`); no consumer-visible artifact, no Bazel analog needed. |
| `jar`              | refused (`unsupported-meson-target-type`) | JVM toolchain not modeled in v1. |

For each `target_sources` cc entry:

- **Sources**: each absolute path is projected against
  `--source-root`. Sources outside the source tree (e.g. ones
  pulled from a subproject) trigger a refusal.
- **Includes**: each `-I<dir>` argument from `parameters`. The
  meson-injected build-dir entries (`<build>/...p`) are dropped;
  source-tree entries are projected and emitted as `Includes`.
- **Defines**: each `-D<NAME>[=<VAL>]` becomes a `Defines` entry.
  meson's auto-injected `-D_FILE_OFFSET_BITS=64` family is kept
  (it's a real semantic flag).
- **Copts**: any flag that isn't `-I` or `-D`, with one exception:
  flags Bazel's cc toolchain emits unconditionally
  (`-fPIC`/`-fpic`/`-fPIE`/`-fpie` and the
  `-fdiagnostics-color=*` family — see `isToolchainHandledFlag` in
  `lower.go`) are dropped. Preserving meson's verbatim copy would
  duplicate the toolchain's emission in every cc_* rule's copts
  for no semantic gain.

For the linker entry within `target_sources`:

- Bare archive references — `libfoo.a` / `libfoo.so` / SONAME-
  versioned `libfoo.so.1.2.3` shapes — are matched against
  in-project archive outputs by basename. Hits become `Deps`
  entries; misses pass through into `LinkOpts` (so the action
  still resolves at link time when the archive is on the system
  path).
- meson-injected linker defaults (`-Wl,--as-needed`,
  `-Wl,--no-undefined`, `-Wl,-soname,*`, `-shared`, `-fPIC`) are
  filtered — Bazel's cc toolchain emits the canonical form.
- All other flags go into `LinkOpts`.
- v1 doesn't parse `-l<name>` linker args. External library
  resolution flows through the `dependencies` field
  (intro-dependencies.json's `compile_args` / `link_args` fold
  inline; cross-element binds resolve via the imports manifest
  in `fillDeps`). If a future fixture exposes raw `-l`
  references that need `LookupLinkLibrary`-style recovery
  (mirroring kind:cmake), wire it in `fillLinkInfo`.

## Cross-element dependency resolution

Mirrors the kind:cmake imports manifest contract
(`internal/manifest/imports.go`). A meson target's
`dependencies` list (names like `glib-2.0`) is matched against
the consumer-side `imports.json` rendered by write-a; on hit,
the matched Bazel label flows into `Deps`. On miss, a Tier-1
`unresolved-meson-dependency` failure surfaces (matching the
cmake converter's behaviour for unbound `find_package` results).

## What v1 doesn't cover (typed refusals)

| Pattern | Failure code | Notes |
|---------|--------------|-------|
| `subproject('foo')` declarations | `unsupported-meson-subproject` | Targets with non-null `subproject` field. |
| `run_command()` reading host state | inherited from `intro-targets.json` (no signal) | If the configure pass succeeds, we lower whatever introspect produced. |
| `find_program('foo')` for non-stdlib tools | not modeled | Bound through the configure step's failure. |
| `meson.add_install_script()` | ignored | Install scripts are configure-time arbitrary code; we don't lower them. |
| Cross-compilation cross-files | not modeled | v1 always uses the host toolchain. |
| `custom_target` with multi-COMMAND or run_command-shaped | `unsupported-meson-custom-target` | Same triage rationale as cmake's execute_process classifier. |

## write-a integration

Mirrors the autotools-native opt-in shape: a global
`mesonConfig.convertBin` populated from `--convert-element-meson
<path>`. When unset, `kind:meson` falls back to the historical
pipelineHandler (coarse `meson configure + ninja + meson install`
genrule) — keeps existing operator flows working. When set, the
handler renders project A like kind:cmake does (per-element
genrule invoking `//tools:convert-element-meson` against a
staged source tree, producing `BUILD.bazel.out` + a
`pkg-config-bundle.tar`); project B writes the
`BUILD_NOT_YET_STAGED` placeholder that the driver script
overwrites.

## Pkg-config bundle (deferred to v2)

The cmake handler emits a `cmake-config-bundle.tar` that
downstream cmake elements stage into their `CMAKE_PREFIX_PATH`.
Meson's analog is a pkg-config tree (`prefix/lib/pkgconfig/<pkg>.pc`)
that downstream meson elements consume via
`PKG_CONFIG_PATH`. v1 emits an empty bundle.tar (so the
genrule's declared output exists); the synthesis lands in a
follow-up.

## Tests + render gate

- Unit tests for the introspection lowering live in
  `converter/cmd/convert-element-meson/lower_test.go`: synthetic
  `Introspect` payloads exercise the static-lib + executable
  shape, the `unsupported-meson-*` refusal codes, the
  `threads → -pthread` inline-fold path, the build-dir filter's
  sibling-collision regression, and `renderCustomCmd`'s
  `@INPUT@` / `@OUTPUT@` validation rules.
- Write-a-side handler tests in
  `cmd/write-a/handler_meson_test.go` cover both the pipeline-
  shape fallback (when `--convert-element-meson` is unset) and
  the native-render shape (`//tools:convert-element-meson`
  invocation, `BUILD.bazel.out` + `pkg-config-bundle.tar` outs,
  source staging into project A and project B).
- Render gate `scripts/meta-meson.sh` mirroring `meta-hello.sh`:
  drives write-a end-to-end + a standalone-converter assertion
  against `testdata/meta-project/meson-greet/`. The bazel-build
  half self-skips when bazel < 7 (no bzlmod) is on PATH or when
  bazel/meson are missing entirely; the render-half assertions
  always run. Asserts the converter produces `cc_library` for
  the static lib, `cc_binary` for the executable, and (when both
  bazel ≥ 7 and meson are present) the smoke binary links + runs.
