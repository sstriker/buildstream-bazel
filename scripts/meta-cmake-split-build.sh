#!/bin/sh
# meta-cmake-split-build.sh — end-to-end gate for the --split-packages
# orchestrator delivery (the cmake_split_convert TreeArtifact rule).
#
# Drives the write-a + Bazel path against testdata/meta-project/split-cmake/,
# a kind:cmake element whose CMakeLists pulls in a sub-CMakeLists via
# add_subdirectory (toplib at the root, util under src/util, headers
# under include/). With --split-packages the element is converted by the
# cmake_split_convert custom rule, whose action emits one BUILD.bazel per
# directory as a TreeArtifact directory.
#
# The gate asserts:
#   1. Render: write-a emits the cmake_split_convert rule (not the
#      single-BUILD genrule) in project A's element BUILD.
#   2. Bazel-build A: `bazel build :<name>_converted` runs the converter
#      under the action and materializes the TreeArtifact with one
#      BUILD.bazel per sub-package (root + src/util + include).
#   3. stage-b merges the TreeArtifact tree into project B.
#   4. Bazel-build B: the split tree compiles with real cc_* rules — the
#      synthesized include/ header library carries the cross-package
#      include path so toplib/util compile and link.
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-cross-cmake.sh; sandboxed environments without
# bcr.bazel.build egress can point bazel at github.com via a registry
# override (and a JVM truststore for the TLS-inspection CA).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake
CGO_ENABLED=0 go build -o "$bin_dir/stage-b" ./cmd/stage-b

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/split-cmake/subdir-library.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --split-packages

# Render-phase checks: the element is converted by the split rule.
elem_build="$A/elements/subdir-library/BUILD.bazel"
if [ ! -f "$elem_build" ]; then
    echo "meta-cmake-split-build: missing rendered project A element BUILD" >&2
    exit 1
fi
if ! grep -q 'cmake_split_convert(' "$elem_build"; then
    echo "meta-cmake-split-build: element BUILD missing cmake_split_convert rule" >&2
    cat "$elem_build" >&2
    exit 1
fi
echo "meta-cmake-split-build: render OK"

# Bazel-availability gating (mirrors meta-cross-cmake.sh).
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-cmake-split-build: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
major=$("$BZL" --version 2>/dev/null | sed -n 's/^bazel \([0-9]*\).*/\1/p')
if [ -z "$major" ] || [ "$major" -lt 9 ]; then
    echo "meta-cmake-split-build: render OK; bazel < 9 (the bzlmod + load() floor), skipping build phase"
    exit 0
fi
for tool in cmake ninja; do
    if ! command -v "$tool" >/dev/null; then
        echo "meta-cmake-split-build: render OK; $tool not on PATH, skipping build phase"
        exit 0
    fi
done

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"

run_bazel() {
    workspace="$1"
    shift
    cmd="$1"
    shift
    # shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

# === Pass 1: build project A's cmake_split_convert TreeArtifact. ===
run_bazel "$A" build //elements/subdir-library:subdir-library_converted 2>&1 | tail -10
pkgs="$A/bazel-bin/elements/subdir-library/subdir-library_converted/packages"
for want in \
    "BUILD.bazel" \
    "src/util/BUILD.bazel" \
    "include/BUILD.bazel"; do
    if [ ! -f "$pkgs/$want" ]; then
        echo "meta-cmake-split-build: TreeArtifact missing $want" >&2
        find "$pkgs" -type f >&2 2>/dev/null || true
        exit 1
    fi
done
# The cross-package wiring: toplib deps on the sub-package + header lib.
if ! grep -q '//elements/subdir-library/src/util' "$pkgs/BUILD.bazel"; then
    echo "meta-cmake-split-build: root BUILD missing cross-package src/util dep" >&2
    cat "$pkgs/BUILD.bazel" >&2
    exit 1
fi
if ! grep -q 'include_headers' "$pkgs/include/BUILD.bazel"; then
    echo "meta-cmake-split-build: include/ BUILD missing synthesized header library" >&2
    exit 1
fi
echo "meta-cmake-split-build: project A built the per-package TreeArtifact"

# === Stage A's TreeArtifact into B. ===
"$bin_dir/stage-b" --project-a "$A" --project-b "$B" >/dev/null
for want in \
    "elements/subdir-library/BUILD.bazel" \
    "elements/subdir-library/src/util/BUILD.bazel" \
    "elements/subdir-library/include/BUILD.bazel"; do
    if [ ! -f "$B/$want" ]; then
        echo "meta-cmake-split-build: stage-b did not stage $want into project B" >&2
        exit 1
    fi
done

# === Pass 2: the split tree compiles in project B. ===
run_bazel "$B" build \
    //elements/subdir-library:toplib \
    //elements/subdir-library/src/util:util \
    //elements/subdir-library/include:include_headers 2>&1 | tail -10

echo "meta-cmake-split-build: ok (write-a --split-packages render + bazel-build TreeArtifact + stage-b merge + project B compiles the split tree)"
