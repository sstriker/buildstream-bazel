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
lifts a raw flag into `features = [...]` **only when the toolchain
actually backs the matching feature** — so a lift can never
silently drop a flag onto a feature no toolchain implements.
Against the converter's generated cc_toolchain, the default-lifted
set is:

| Flag | Lifted to |
|---|---|
| `-fPIC` / `-fpic` | `features = ["pic"]` |
| `-flto` | `features = ["lto"]` |
| `-fsanitize=address` / `thread` / `memory` / `undefined` | `features = ["asan"]` / `["tsan"]` / `["msan"]` / `["ubsan"]` |

`-fvisibility=hidden`, `-fvisibility-inlines-hidden`, and
`-fsanitize=leak` map to feature *names* the converter recognises
(`visibility_hidden`, `visibility_inlines_hidden`, `lsan`), but the
generated toolchain — and bazel's autodetected default — **don't
define those features**, so by default these flags **stay raw
copts** rather than being lifted onto a no-op. If your toolchain
*does* declare them, see
[`--toolchain-features-from`](#reading-your-toolchains-real-features---toolchain-features-from)
below to lift them too. The full flag→feature-name table lives in
`converter/internal/toolchainfeature/Feature`; which of those
names are actually lifted is gated by the toolchain's vocabulary.

**The warning-style flags (`-W*`, `-pedantic`, `-fno-rtti`, etc.)
have no feature mapping at all** and always stay in copts —
lifting them would need both a `toolchainfeature.Feature` mapping
*and* a toolchain that declares the feature.

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

## Reading your toolchain's real features (`--toolchain-features-from`)

Options 2 and 3 are the operator owning the gap by hand. This flag is
the converter-assisted version of Option 3: point `convert-element-cmake`
at your real toolchain and the lift gates on **its** declared feature
vocabulary instead of the converter's generated default.

```
convert-element-cmake --toolchain-features-from path/to/toolchains \
  ...
```

The path is either a single `cc_toolchain_config.bzl` or a `toolchains/`
directory (its top-level `*.bzl` are parsed and their features unioned).
The converter statically parses the Starlark
([`converter/internal/toolchainscan`](../converter/internal/toolchainscan/)),
enumerates the feature names your toolchain declares, and lifts a flag
only when the matching feature is among them (plus the built-in `pic`).

Concretely: if your toolchain declares a `visibility_hidden` feature,
`-fvisibility=hidden` now lifts (it stays raw under the default
vocabulary); if it does **not** declare `asan`, `-fsanitize=address`
stays a raw copt. The flag changes *which known features are eligible* —
it does **not** add new flag→feature mappings, so warning-style flags
without a `toolchainfeature.Feature` entry still stay in copts.

If your targets can resolve to more than one toolchain (e.g. several
kits), point the flag at the one whose vocabulary is the **intersection**
you want lifted — lifting a feature some resolvable toolchain lacks would
re-introduce the silent-drop those features are conservative about.

### Supported `feature()` syntax

Enumeration is a parse, not an evaluation, so a feature is recognised
when its **name is a string literal** in a direct `feature()` call (the
`@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl` form):

```python
feature(name = "opt")            # keyword arg (the norm)
feature("opt")                   # first positional arg
feature(name = "asan", flag_sets = [...])
```

Names that only exist after Starlark runs are **not** seen:

- computed: `feature(name = "san_" + mode)`, `feature(name = var)`
- names generated in a loop/comprehension over a non-literal list
- features built by a wrapper function (the literal lives at the
  wrapper's call site, not the `feature()` call)

Resolving those would need full rule evaluation (a `ctx` + `cc_common`
environment), which is out of scope. Hand-written and
`cc_toolchain_config_lib`-style toolchains name features with literals,
so the parse is exact for them; for anything dynamic, declare the
feature with a literal name (or omit the flag and keep the generated
default vocabulary).

Passing the flag is a deliberate opt-in, so it gates the lift on exactly
what the parse found — **even if that's nothing**. If the scan turns up no
literal `feature()` names (e.g. a wrapper-based toolchain), the lift is
gated to only the built-in `pic` rather than silently falling back to the
generated default (which could lift onto features your toolchain doesn't
define); the converter prints a warning naming the path so the under-lift
isn't a surprise.

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
