# sanitizer-features — bridging cmake sanitizer configs and Bazel features

End-to-end example for the convention documented in
[`docs/design/sanitizer-as-feature.md`](../../docs/design/sanitizer-as-feature.md):

- `cmake-side/` — a CMakeLists.txt that defines per-config
  sanitizer flag sets the cmake way (`CMAKE_C_FLAGS_ASAN`, etc.).
  This is what the converter consumes.
- `toolchain/` — a cc_toolchain feature definition the operator
  hooks into project B. The converter's output is sanitizer-
  agnostic; this toolchain carries the flag sets the
  `--features=asan` / `--features=tsan` opt-ins activate.
- `bazel-side/` — a sample BUILD.bazel showing how downstream
  Bazel targets pick up the feature (per-build via flag,
  per-package via `package(features=…)`, or per-target via
  `cc_library(features=…)`).

Together these show the operator-side wiring for the residual
sanitizer-as-feature work item under
[`generator-parity-uplift.md`](../../docs/design/generator-parity-uplift.md)
Phase 5.

## Running the example

The example is **descriptive**, not a checked-in test fixture —
the converter's `make e2e-*` gates don't exercise it. Operators
copy the relevant pieces into their own projects:

```sh
# 1. Add the toolchain features to your project's MODULE.bazel / toolchain/.
cp -r examples/sanitizer-features/toolchain my-project/toolchain/

# 2. Convert your cmake project with --build-types listing
#    sanitizer configs. The converter recognizes the names
#    (ASan, TSan, ...) and skips the per-config select that
#    would otherwise produce the anti-pattern Phase 7's
#    audit catches.
build/bin/convert-element-cmake \
    --source-root my-project/src \
    --build-types=Release,ASan,TSan \
    --out-build my-project/BUILD.bazel

# 3. Build with the sanitizer feature active.
cd my-project && bazel build --features=asan //...
```

## What's NOT in the converter's output

The emitted BUILD.bazel does NOT contain:

- `cc_library(features = ["asan"])` annotations — the converter
  is sanitizer-agnostic so the same output works for every
  variant.

- `select({"//config:asan": ["-fsanitize=address"]})` on
  `copts` / `linkopts` — that's the anti-pattern Phase 7's
  `sanitizer-select-not-feature` audit finding catches.

- `config_setting(name = "asan")` declarations — the toolchain
  features replace the need for these.

The cmake `CMAKE_C_FLAGS_ASAN` value from `cmake-side/` flows
INTO the toolchain feature definitions, not into per-rule
attributes. Operators tune the sanitizer flags by editing
`toolchain/features.bzl`, not by re-converting.

## Why the split — see the design doc

[`docs/design/sanitizer-as-feature.md`](../../docs/design/sanitizer-as-feature.md)
covers the rationale (one feature definition per sanitizer
shared across all converted projects, vs. N per-project flag
sets duplicating the same `-fsanitize=address`).
