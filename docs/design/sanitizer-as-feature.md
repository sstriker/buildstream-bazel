# Sanitizer-as-feature: routing cmake sanitizer configs to Bazel features

The cmake idiom for sanitizer / instrumentation variants is a
**per-config flag set**: the project defines `CMAKE_C_FLAGS_ASAN`,
`CMAKE_EXE_LINKER_FLAGS_ASAN`, etc., then operators build with
`-DCMAKE_BUILD_TYPE=ASan` (single-config) or
`-DCMAKE_CONFIGURATION_TYPES=Release;ASan;TSan` (multi-config).

The Bazel idiom is a **cc_toolchain feature**: each sanitizer is
a named `feature {}` in the toolchain definition, with `flag_set
{}` entries declaring the compile/link flags. Operators opt in
globally with `--features=asan` or per-target with
`cc_library(features = ["asan"])`.

This doc explains how the converter bridges the two conventions
and what the operator side has to wire up.

## What the converter does automatically

Phase 5 of the generator-parity uplift
([`generator-parity-uplift.md`](generator-parity-uplift.md))
handles the cmake side:

- `cmakerun.Options.BuildTypes []string` switches the cmake
  generator to `Ninja Multi-Config` so one configure pass
  produces a codemodel reply with per-sanitizer Configuration
  entries.

- `fileapi.Reply.TargetsByConfig` carries per-config target data
  (one slot per `CMAKE_CONFIGURATION_TYPES` entry).

- `configfold.SanitizerFeature(name)` recognizes the standard
  sanitizer-shaped config names: `ASan` / `AddressSanitizer`,
  `TSan` / `ThreadSanitizer`, `MSan` / `MemorySanitizer`,
  `UBSan` / `UndefinedBehaviorSanitizer`, `LSan` /
  `LeakSanitizer`, `Coverage` / `cov` / `gcov`, `LTO` (plus
  suffix variants like `ReleaseLTO`).

- `lower/multiconfig.go`'s cross-config fold **skips** these
  sanitizer-shaped configs deliberately — emitting a
  `select({"//config:asan": ["-fsanitize=address"], …})` on
  every cc_library would defeat the feature convention. The
  audit pass (Phase 7,
  [`bazelidiom`](../../converter/internal/bazelidiom/audit.go))
  catches anyone who hand-rolls this anti-pattern, surfacing
  it as the `sanitizer-select-not-feature` finding.

## What the operator wires up

The cc_toolchain feature definitions live outside the converter's
output — they're operator-controlled, often shared across many
converted projects. The pattern follows Bazel's
[`@rules_cc//cc:toolchains`](https://bazel.build/rules/lib/builtins/cc_toolchain.html)
conventions; the [`examples/sanitizer-features/`](../../examples/sanitizer-features/)
directory is a runnable starting point.

### Feature naming convention

Use the lowercase short name `configfold.SanitizerFeature` returns
so future converter slices that auto-route can name the feature
by string match:

| cmake config | Bazel feature name |
|---|---|
| `ASan` / `AddressSanitizer` | `asan` |
| `TSan` / `ThreadSanitizer` | `tsan` |
| `MSan` / `MemorySanitizer` | `msan` |
| `UBSan` / `UndefinedBehaviorSanitizer` | `ubsan` |
| `LSan` / `LeakSanitizer` | `lsan` |
| `Coverage` / `cov` / `gcov` | `coverage` |
| `LTO` / `*LTO` | `lto` |

### Feature definition shape

Each feature opens with a `feature {}` block; per-action `flag_set`
entries declare which compiler invocations the flags fire for.
Sanitizers typically need both compile-time flags (the
`c_compile` + `cpp_compile` actions) and link-time flags
(`cpp_link_executable` + `cpp_link_dynamic_library`).

The [`examples/sanitizer-features/toolchain/features.bzl`](../../examples/sanitizer-features/toolchain/features.bzl)
file in this repo carries the full pattern for each sanitizer;
the gist for `asan`:

```python
asan_feature = feature(
    name = "asan",
    enabled = False,  # opt-in via --features=asan
    flag_sets = [
        flag_set(
            actions = [
                ACTION_NAMES.c_compile,
                ACTION_NAMES.cpp_compile,
            ],
            flag_groups = [flag_group(flags = [
                "-fsanitize=address",
                "-fno-omit-frame-pointer",
                "-g",
            ])],
        ),
        flag_set(
            actions = [
                ACTION_NAMES.cpp_link_executable,
                ACTION_NAMES.cpp_link_dynamic_library,
            ],
            flag_groups = [flag_group(flags = ["-fsanitize=address"])],
        ),
    ],
)
```

### Mutually-exclusive sanitizers

ASan / TSan / MSan can't coexist (the runtime libraries collide).
Bazel feature `implies` / `requires` / mutually-exclusive groups
encode the constraint:

```python
# Inside cc_common.create_cc_toolchain_config_info(...):
features = [
    asan_feature,
    tsan_feature,
    msan_feature,
    # ... + a sentinel feature group that conflicts pairwise:
    feature(
        name = "no_other_sanitizer",
        # Featured-set group; activating one excludes the others.
        # See examples/ for the full shape.
    ),
]
```

### Operator opt-in

Three opt-in paths, in order of locality:

1. **Per-build**: `bazel build --features=asan //…`
2. **Per-rule**: `cc_library(features = ["asan"])` in the BUILD
3. **Per-package**: `package(features = ["asan"])` at the BUILD
   file head (applies to every rule in the package)

The converter never emits any of these — the output is
sanitizer-agnostic. The operator picks the level appropriate
for their workflow (typically per-build for CI matrices).

## Why this split — converter vs. operator

The cmake `CMAKE_<LANG>_FLAGS_ASAN` value varies per-project:
one project might use `-fsanitize=address -O1`, another
`-fsanitize=address -O0 -fno-omit-frame-pointer`, a third the
cmake-Boilerplate `-fsanitize=address` only. The converter could
naively copy each project's flags into a per-config select, but
that produces N feature definitions for the same sanitizer
(one per converted element) — exactly the duplication the
shared cc_toolchain is designed to eliminate.

The cleaner shape is: the converter recognizes the variant by
name, leaves the per-rule emission sanitizer-agnostic, and the
operator's shared toolchain owns the flag set. The Bazel feature
mechanism is the right home for "this flag applies to every cc
target compiled under this toolchain"; per-rule selects don't
compose that way.

## Converter-generated features.bzl

When the operator passes `--out-sanitizer-features <path>`
alongside `--build-types`, the converter extracts cmake's
per-config `CMAKE_<LANG>_FLAGS_<CONFIG>` /
`CMAKE_<TYPE>_LINKER_FLAGS_<CONFIG>` cache entries and renders
a matching `features.bzl` at the requested path.

```sh
convert-element-cmake \
    --source-root my-project/src \
    --build-types=Release,ASan,TSan,UBSan \
    --out-build my-project/BUILD.bazel \
    --out-sanitizer-features my-project/toolchain/sanitizer-features.bzl
```

Output shape:

- `feature(name = "asan", enabled = False, flag_sets = [...])`
  per sanitizer-shaped config (recognized via
  `configfold.SanitizerFeature`).
- Per-language `flag_set` actions: C → `ACTION_NAMES.c_compile`;
  CXX → `cpp_compile` + `cpp_module_compile`; ASM →
  `assemble` + `preprocess_assemble`. Languages with no cmake
  flag entries omit the corresponding flag_set.
- Link `flag_set` actions: `cpp_link_executable` +
  `cpp_link_dynamic_library` + `cpp_link_nodeps_dynamic_library`.
  cmake's `CMAKE_EXE_LINKER_FLAGS_<CONFIG>` and
  `CMAKE_SHARED_LINKER_FLAGS_<CONFIG>` merge with first-occurrence
  dedup — most sanitizers' link flags are identical between
  the two, and the union shape is safer than two separate
  flag_sets that could drift.
- Final `SANITIZER_FEATURES = [...]` list operators thread into
  their `cc_common.create_cc_toolchain_config_info(features=…)`
  call.
- Byte-stable across runs; re-generates whenever the operator
  re-runs convert-element-cmake. Manual edits get overwritten —
  copy the list into a separate file if you need to customize
  beyond what cmake records.

The generated file pairs with the `toolchain/features.bzl` in
[`examples/sanitizer-features/`](../../examples/sanitizer-features/);
operators can compare their cmake-derived output against the
hand-curated reference to spot drift between the project's
sanitizer recipe and the convention.

### Future extensions

- Drift warning: when the operator's existing `features.bzl`
  already declares a sanitizer feature whose flag set differs
  from the cmake-recorded one, emit a `# WARNING: cmake's <name>
  flags differ` comment in the generated file.
- Mutual-exclusion sentinel generation: derive the
  `_sanitizer_runtime` sentinel feature from the recognized
  set so ASan/TSan/MSan combinations fail cleanly at toolchain
  config time.

Both auxiliary — the core pattern (operator-defined features,
converter-recognized config names, audit catches hand-rolled
selects) holds without them.

## See also

- [`generator-parity-uplift.md`](generator-parity-uplift.md) —
  Phase 5 specification.
- [`examples/sanitizer-features/`](../../examples/sanitizer-features/) —
  runnable example with toolchain definitions + sample
  CMakeLists.txt.
- [`bazel.build/docs/cc-toolchain-config-reference`](https://bazel.build/docs/cc-toolchain-config-reference) —
  cc_toolchain feature configuration reference.
