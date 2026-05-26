"""Sanitizer cc_toolchain feature definitions.

Pair with a cc_toolchain_config rule that calls
cc_common.create_cc_toolchain_config_info(... features = SANITIZER_FEATURES + …).
Operators activate one via --features=asan (etc.) or per-target
features = [...].

The naming maps 1:1 onto configfold.SanitizerFeature so a future
converter slice that auto-emits features = [...] knows the
expected string.

See ../../docs/design/sanitizer-as-feature.md for the rationale
and the operator wiring guide.
"""

load("@bazel_tools//tools/build_defs/cc:action_names.bzl", "ACTION_NAMES")
load(
    "@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl",
    "feature",
    "flag_group",
    "flag_set",
)

# Compile actions every sanitizer instruments.
_SANITIZER_COMPILE_ACTIONS = [
    ACTION_NAMES.c_compile,
    ACTION_NAMES.cpp_compile,
    ACTION_NAMES.cpp_module_compile,
    ACTION_NAMES.assemble,
    ACTION_NAMES.preprocess_assemble,
]

# Link actions every sanitizer needs to pass its runtime to.
_SANITIZER_LINK_ACTIONS = [
    ACTION_NAMES.cpp_link_executable,
    ACTION_NAMES.cpp_link_dynamic_library,
    ACTION_NAMES.cpp_link_nodeps_dynamic_library,
]

def _sanitizer_feature(name, flags):
    """Build a feature {} with the standard compile+link split.

    flags applies to BOTH compile and link actions — matches the
    typical cmake pattern (CMAKE_C_FLAGS_ASAN +
    CMAKE_EXE_LINKER_FLAGS_ASAN both carry -fsanitize=address).
    Add `-fno-omit-frame-pointer` / `-g` etc. as additional
    compile-only flags via a custom feature if the project needs
    finer control.
    """
    return feature(
        name = name,
        enabled = False,  # opt-in via --features=<name>
        flag_sets = [
            flag_set(
                actions = _SANITIZER_COMPILE_ACTIONS,
                flag_groups = [flag_group(flags = flags + [
                    "-fno-omit-frame-pointer",
                    "-g",
                ])],
            ),
            flag_set(
                actions = _SANITIZER_LINK_ACTIONS,
                flag_groups = [flag_group(flags = flags)],
            ),
        ],
    )

asan_feature = _sanitizer_feature(
    name = "asan",
    flags = ["-fsanitize=address"],
)

tsan_feature = _sanitizer_feature(
    name = "tsan",
    flags = ["-fsanitize=thread"],
)

msan_feature = _sanitizer_feature(
    name = "msan",
    flags = [
        "-fsanitize=memory",
        # MSan needs origin tracking to surface useful diagnostics.
        "-fsanitize-memory-track-origins=2",
    ],
)

ubsan_feature = _sanitizer_feature(
    name = "ubsan",
    flags = ["-fsanitize=undefined"],
)

lsan_feature = _sanitizer_feature(
    name = "lsan",
    flags = ["-fsanitize=leak"],
)

# Coverage isn't a sanitizer per se, but it shares the
# "per-build flag overlay" shape, so it lives alongside.
coverage_feature = feature(
    name = "coverage",
    enabled = False,
    flag_sets = [
        flag_set(
            actions = _SANITIZER_COMPILE_ACTIONS,
            flag_groups = [flag_group(flags = [
                "--coverage",
                "-O0",
                "-g",
            ])],
        ),
        flag_set(
            actions = _SANITIZER_LINK_ACTIONS,
            flag_groups = [flag_group(flags = ["--coverage"])],
        ),
    ],
)

# LTO needs different actions (the link is what matters most;
# compile-time -flto enables the IR emission).
lto_feature = feature(
    name = "lto",
    enabled = False,
    flag_sets = [
        flag_set(
            actions = _SANITIZER_COMPILE_ACTIONS,
            flag_groups = [flag_group(flags = ["-flto"])],
        ),
        flag_set(
            actions = _SANITIZER_LINK_ACTIONS,
            flag_groups = [flag_group(flags = ["-flto"])],
        ),
    ],
)

# Mutual exclusion: ASan / TSan / MSan runtimes can't coexist
# in the same binary. The sentinel feature is OFF by default;
# each of asan/tsan/msan implies it. Two simultaneously-active
# sanitizers would both try to imply the sentinel, which Bazel
# treats as a conflict — fails the build with a clear message.
_sanitizer_runtime_sentinel = feature(
    name = "_sanitizer_runtime",
    enabled = False,
    # The implies field is set in SANITIZER_FEATURES below by
    # threading each sanitizer's feature() through a re-bind
    # because feature.implies isn't mutable post-construction.
)

# The exported list operators thread into
# cc_common.create_cc_toolchain_config_info(features = ...).
# Order matters for Bazel's feature-merge: the sanitizers come
# first so per-build --features=asan wins over any toolchain
# default that might enable a competing variant.
SANITIZER_FEATURES = [
    asan_feature,
    tsan_feature,
    msan_feature,
    ubsan_feature,
    lsan_feature,
    coverage_feature,
    lto_feature,
]
