# sanitizer-features — bridging cmake sanitizer configs and Bazel features

End-to-end example for the converter convention: cmake sanitizer
configs (`-DCMAKE_BUILD_TYPE=ASAN` with `CMAKE_C_FLAGS_ASAN`, etc.)
lower to a Bazel cc_toolchain `--features=asan` opt-in rather than
to per-rule `select()` arms.

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

## Running the example

The example is **descriptive**, not a checked-in test fixture —
the converter's `make e2e-*` gates don't exercise it. Operators
copy the relevant pieces into their own projects.

### Two paths to the toolchain features file

**Path A — hand-curated (`toolchain/features.bzl`)**: copy the
checked-in file as-is. Use this when the converter-generated
flags aren't quite what you want (e.g. you prefer
`-O1` over the project's `-O2`, or you want to add
`-fno-omit-frame-pointer` everywhere).

```sh
cp -r examples/sanitizer-features/toolchain my-project/toolchain/
```

**Path B — converter-generated (`--out-sanitizer-features`)**:
let the converter extract `CMAKE_<LANG>_FLAGS_<CONFIG>` from
the cmake cache and emit a matching `features.bzl`. Byte-stable
across runs; re-generates on each conversion.

```sh
# 1. Convert + generate the sidecar in one pass.
build/bin/convert-element-cmake \
    --source-root my-project/src \
    --build-types=Release,ASan,TSan,UBSan \
    --out-build my-project/BUILD.bazel \
    --out-sanitizer-features my-project/toolchain/features.bzl

# 2. Build with the sanitizer feature active.
cd my-project && bazel build --features=asan //...
```

The generated `features.bzl` has the same shape as the
hand-curated one in this directory — it just derives the flag
values from cmake's cache rather than copying the example's
defaults. Operators who customize the cmake side
(`CMAKE_C_FLAGS_ASAN = "-fsanitize=address -O1 -fno-omit-frame-pointer ..."`)
get a `features.bzl` that mirrors those exact flags.

### Common shape regardless of path

In both paths the **converted output** (the `bazel-side/BUILD.bazel`
the converter emits) stays sanitizer-agnostic:

## What's NOT in the converter's output (BUILD.bazel)

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

## Why the split

One cc_toolchain feature definition per sanitizer, shared across
all converted projects, vs. N per-project flag sets each
duplicating the same `-fsanitize=address`. The converter's output
stays sanitizer-agnostic; the operator's toolchain carries the
flag sets. Per-project sanitizer toggles become `--features=asan` /
`--features=tsan` opt-ins at `bazel build` time, with the same flag
shape across every converted project.
