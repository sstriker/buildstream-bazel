# Operator recommendations: lifting common copts to cc_toolchain features

When `convert-element-cmake` lowers a large cmake project, every
`cc_library` / `cc_binary` / `cc_test` typically carries the same
20+ warning-style copts that cmake folded into `CMAKE_CXX_FLAGS`
at configure time. The converter emits these verbatim because
they don't have first-class `cc_toolchain` features mapped in
[`converter/internal/toolchainfeature`](../converter/internal/toolchainfeature/).

This document explains the pattern, what the converter handles
automatically, and what the operator owns.

## The pattern

LLVM's converted BUILD shows the shape clearly — every one of
308 `cc_library` / `cc_binary` rules carries this prefix:

```python
copts = [
    "-fno-semantic-interposition",
    "-Werror=date-time",
    "-fno-lifetime-dse",
    "-Wall",
    "-Wextra",
    "-Wno-unused-parameter",
    "-Wwrite-strings",
    "-Wcast-qual",
    "-Wno-missing-field-initializers",
    "-Wno-long-long",
    "-Wimplicit-fallthrough",
    "-Wno-comment",
    "-pedantic",
    "-Wsuggest-override",
    "-Wno-maybe-uninitialized",
    "-Wno-nonnull",
    "-Wno-class-memaccess",
    # ... + project-specific copts
]
```

The same 17 entries × 308 rules ≈ 5200 lines of byte-for-byte
identical copts. Bazel accepts the verbose form, but the
maintenance and re-readability of the converted BUILD suffers.

## What the converter handles automatically

The `liftRawFeatureFlags` post-pass
([`converter/internal/lower/lift_feature_flags.go`](../converter/internal/lower/lift_feature_flags.go))
already lifts these into `features = [...]`:

| Flag | Lifted to |
|---|---|
| `-fPIC` | `features = ["pic"]` |
| `-fvisibility=hidden` | `features = ["hidden_visibility"]` |
| `-fvisibility-inlines-hidden` | `features = ["hidden_inlines_visibility"]` |
| `-flto` / `-flto=thin` / `-flto=full` | `features = ["lto"]` |
| `-fsanitize=<x>` family | `features = ["asan"]` / `["tsan"]` / etc. |

These flags have established cc_toolchain feature equivalents in
stock `rules_cc` and `rules_cc_toolchains` — the lift table is
mapped in `converter/internal/toolchainfeature/Feature`.

**The warning-style flags (`-W*`, `-pedantic`, `-fno-rtti`, etc.)
don't have stock cc_toolchain features.** They stay in copts
because mapping them would require the operator to declare a
matching feature; without coordination, an unmapped feature name
is a no-op and the flag silently disappears.

## What the operator owns (and what to do)

Pick one of three patterns:

### Option 1 — leave as-is

The duplicated copts work correctly. Bazel doesn't reject them,
and the per-rule copts are easy to grep / modify when an
operator-driven refactor needs to override one. This is the
zero-config default; nothing for the operator to do.

**Trade-off**: BUILD.bazel is verbose. Diff noise is high when
the converter updates flag sets between cmake versions.

### Option 2 — wrap rules in a Starlark macro

Move the common copts into a project-scoped Starlark macro:

```python
# //tools/cc:macros.bzl
load("@rules_cc//cc:defs.bzl", "cc_library")

PROJECT_COMMON_COPTS = [
    "-fno-semantic-interposition",
    "-Werror=date-time",
    "-fno-lifetime-dse",
    "-Wall",
    "-Wextra",
    # ... rest of the 17
]

def project_cc_library(name, copts = [], **kwargs):
    cc_library(
        name = name,
        copts = PROJECT_COMMON_COPTS + copts,
        **kwargs,
    )
```

Then run a one-time sed/buildifier pass on the converted
BUILD.bazel to replace `cc_library(` → `project_cc_library(` and
strip the common copts from each rule's `copts` list.

**Trade-off**: the converter doesn't know about your macro, so
re-running `convert-element-cmake` regenerates the verbose form
and you re-apply the sed pass. Works well as a one-shot
post-process when the cmake source has stabilised.

### Option 3 — declare a cc_toolchain feature in project B

The Bazel-idiomatic answer for cross-target compiler flag bundles
is a cc_toolchain feature. Project B (the consumer Bazel workspace
that hosts every converted element) typically has **one
cc_toolchain** the whole workspace shares — or a small handful
when truly different toolchains are needed (e.g. host vs.
cross-compile, with vs. without sanitizers). Adding a feature to
that shared toolchain covers every converted element at once:

```python
# project-B //toolchain/features.bzl
load("@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl", "feature", "flag_set", "flag_group")

PROJECT_WARNINGS_FEATURE = feature(
    name = "project_warnings",
    enabled = True,  # on by default for every consumer
    flag_sets = [
        flag_set(
            actions = ["c_compile", "cpp_compile"],
            flag_groups = [
                flag_group(flags = [
                    "-fno-semantic-interposition",
                    "-Werror=date-time",
                    "-fno-lifetime-dse",
                    "-Wall",
                    "-Wextra",
                    # ... rest
                ]),
            ],
        ),
    ],
)
```

Once the feature is declared and `enabled = True`, every
`cc_library` / `cc_binary` registered against this cc_toolchain
picks up the flags automatically — regardless of how many
converted elements end up consuming it. You can then file an
issue asking the converter to extend `toolchainfeature.Feature`
to recognise the project's specific flag set and emit
`features = ["project_warnings"]` instead of the raw copts.

**Trade-off**: edits to the flag set are workspace-wide. Per-
element overrides go through Bazel's `features = ["-project_warnings"]`
opt-out on the consuming rule.

## Per-source COMPILE_DEFINITIONS — no operator action needed

cmake's `set_source_files_properties(foo.c PROPERTIES
COMPILE_DEFINITIONS "X")` produces correct per-source behavior
**automatically**. cmake puts each affected source in its own
`CompileGroup`; the converter's `splitCompileGroups` lift emits
one sub-`cc_library` per group with the right `defines`:

```python
# input: set_source_files_properties(a.c PROPERTIES COMPILE_DEFINITIONS "X")

cc_library(
    name = "mylib",
    deps = [":mylib_c_0", ":mylib_c_1"],
)
cc_library(
    name = "mylib_c_0",
    srcs = ["a.c"],
    defines = ["X"],
    visibility = ["//visibility:private"],
)
cc_library(
    name = "mylib_c_1",
    srcs = ["b.c"],
    visibility = ["//visibility:private"],
)
```

Same shape works for `COMPILE_OPTIONS` and per-source `LANGUAGE`
overrides.
