# Meson native render — design

Per `ROADMAP.md` "Next: kind:meson native render" and
`docs/fdsdk-coverage-status.md` (12.3% of FDSDK; biggest single
lift remaining after kind:cmake).

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
   source tree ──▶ │ meson setup <src> <bd> │ ──▶ <bd>/meson-info/*.json
                   └────────────────────────┘
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

The `cmd/convert-element-meson/main.go` binary is a sibling of
`converter/cmd/convert-element/main.go` (the cmake converter).
Both consume an external build-system's introspection and
produce IR + BUILD.bazel.out via the same emit package.

## Per-target lowering rules

| Meson `type`       | IR `Kind`                | Notes                                    |
|--------------------|--------------------------|------------------------------------------|
| `static library`   | `KindCCLibrary` + `Linkstatic=true` | `ArtifactName` from `filename` basename. |
| `shared library`   | `KindCCLibrary`          | Defaults to dynamic linking.             |
| `both libraries`   | `KindCCLibrary`          | Single rule (Bazel decides static vs shared via toolchain). |
| `executable`       | `KindCCBinary`           | -                                        |
| `custom`           | `KindGenrule` (best-effort) | Lifted only when the command's argv contains `@INPUT@`/`@OUTPUT@` and no host-probing tools. |
| `run`              | refused (`unsupported-meson-target-type`) | Configure-time / dev-only target. |
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
- **Copts**: any flag that isn't `-I` or `-D`. Color-diagnostics
  flags (`-fdiagnostics-color=always`, `-Winvalid-pch`) and
  `-fPIC` are kept verbatim — Bazel's toolchain may add the same
  flags but duplication is harmless and preserves the meson
  intent.

For the linker entry within `target_sources`:

- `parameters` are split: `-l<name>` references resolve via the
  `depends` list (in-project) or the imports manifest (cross-
  element / system). Bare `libfoo.a` references are matched
  against in-project archive outputs by basename.
- All other flags go into `LinkOpts`.

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

- Unit tests for the introspect parser (`main_test.go` golden
  fixtures under `testdata/meson-introspect/`).
- Render gate `scripts/meta-meson.sh` mirroring `meta-hello.sh`:
  drives write-a + bazel-build (when bazel is present) against
  `testdata/meta-project/meson-greet/`, asserts the converter
  produces `cc_library` for the static lib, `cc_binary` for the
  executable, and the smoke binary links + runs.
