#!/bin/sh
# meta-cmake-cross-package-target-file.sh — acceptance gate for
# PR 2 of cross-package `$<TARGET_FILE:t>` resolution.
#
# Drives the pipeline against testdata/meta-project/cross-package-
# target-file/, which has two kind:cmake elements: producer (a
# STATIC library that exports a cmake-config bundle) and consumer
# (a STATIC library whose CMakeLists.txt uses
# `find_package(producer CONFIG REQUIRED) +
# file(GENERATE OUTPUT tool_path.h CONTENT "...$<TARGET_FILE:producer::producer>...")`).
#
# The gate asserts:
#   1. Render: write-a emits per-element BUILDs with an
#      imports.json on the consumer side mapping
#      producer::producer → //elements/producer:producer.
#   2. Bazel-build of the consumer's converter genrule:
#      cmake's find_package(producer CONFIG) resolves against
#      the staged bundle; the trace captures the file(GENERATE)
#      call; convert-element-cmake's file(GENERATE) lifter
#      sees the cross-package $<TARGET_FILE:producer::producer>
#      reference, looks it up in the imports manifest, and
#      emits a lifted genrule whose cmd carries
#      `--target-file=producer::producer="$(location
#      //elements/producer:producer)"`.
#   3. The refusal-stub tag
#      (`cmake-codegen-genex-cross-package`) does
#      NOT fire — the resolved-lift path replaces the PR 1
#      refusal stub for the manifest-hit case.
#
# Bazel-availability + cmake/ninja gating + META_BAZEL_*_ARGS
# overrides mirror scripts/meta-cross-cmake.sh; sandboxed
# environments without bcr.bazel.build egress can point bazel
# at github.com via a registry override.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/cross-package-target-file/producer.bst \
    --bst testdata/meta-project/cross-package-target-file/consumer.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks.
for want in \
    "elements/producer/BUILD.bazel" \
    "elements/consumer/BUILD.bazel" \
    "elements/consumer/imports.json"; do
    if [ ! -f "$A/$want" ]; then
        echo "meta-cmake-cross-package-target-file: missing rendered file in project A: $want" >&2
        exit 1
    fi
done
if ! grep -q '"bazel_label": "//elements/producer:producer"' "$A/elements/consumer/imports.json"; then
    echo "meta-cmake-cross-package-target-file: consumer imports.json missing producer mapping" >&2
    cat "$A/elements/consumer/imports.json" >&2
    exit 1
fi
echo "meta-cmake-cross-package-target-file: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-cmake-cross-package-target-file: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-cmake-cross-package-target-file: render OK; bazel $($BZL --version | head -1) is < 9, skipping build phase"
    exit 0
fi

# Bazel-build half execs convert-element-cmake which shells to
# cmake + ninja. Skip cleanly if either is missing.
for tool in cmake ninja; do
    if ! command -v "$tool" >/dev/null; then
        echo "meta-cmake-cross-package-target-file: render OK; $tool not on PATH, skipping build phase"
        exit 0
    fi
done

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"
run_bazel() {
    workspace="$1"
    cmd="$2"
    shift 2
    # shellcheck disable=SC2086
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

# Build both elements' converter genrules so the consumer can
# pick up producer's bundle and the trace.
run_bazel "$A" build //elements/producer:producer_converted //elements/consumer:consumer_converted 2>&1 | tail -10
consumer_build="$A/bazel-bin/elements/consumer/BUILD.bazel.out"
if [ ! -f "$consumer_build" ]; then
    echo "meta-cmake-cross-package-target-file: consumer BUILD.bazel.out not produced" >&2
    exit 1
fi

# === The PR 2 assertions ===============================================
#
# 1. The lifted file(GENERATE) genrule's cmd carries the
#    --target-file flag pointing at the manifest-resolved
#    cross-package label, NOT the same-package shorthand.
want_flag='--target-file=producer::producer="$(location //elements/producer:producer)"'
if ! grep -qF -- "$want_flag" "$consumer_build"; then
    echo "meta-cmake-cross-package-target-file: consumer BUILD.bazel.out missing expected --target-file flag" >&2
    echo "want: $want_flag" >&2
    echo "--- consumer BUILD.bazel.out ---" >&2
    cat "$consumer_build" >&2
    exit 1
fi
echo "meta-cmake-cross-package-target-file: --target-file resolved to cross-package label OK"

# 2. The (a) lift's tag IS present.
if ! grep -q '"cmake-codegen-genex-resolved"' "$consumer_build"; then
    echo "meta-cmake-cross-package-target-file: consumer BUILD.bazel.out missing (a) lift tag cmake-codegen-genex-resolved" >&2
    echo "--- consumer BUILD.bazel.out ---" >&2
    cat "$consumer_build" >&2
    exit 1
fi

# 3. The refusal-stub tag must NOT fire — the resolved-lift
#    path supersedes PR 1's refusal stub for manifest-hit cases.
if grep -q '"cmake-codegen-genex-cross-package"' "$consumer_build"; then
    echo "meta-cmake-cross-package-target-file: refusal-stub tag (cmake-codegen-genex-cross-package) fired but should NOT — manifest resolved producer::producer" >&2
    echo "--- consumer BUILD.bazel.out ---" >&2
    cat "$consumer_build" >&2
    exit 1
fi
echo "meta-cmake-cross-package-target-file: refusal-stub tag correctly absent"

echo "meta-cmake-cross-package-target-file: ok"
