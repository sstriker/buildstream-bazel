#!/bin/sh
# meta-cmake-workspace-root.sh — render gate for the zstd-shape
# build/cmake/CMakeLists.txt layout (round-5 of PR #227, commit
# 69786a7).
#
# Stages a temp project mirroring the zstd layout:
#   $tmp/.git/HEAD              ← workspace marker for detectWorkspaceRoot
#   $tmp/lib/demo.c             ← source one level above cmakeSrc
#   $tmp/include/demo.h
#   $tmp/build/cmake/CMakeLists.txt  ← cmake source dir
#
# Drives convert-element-cmake at the deeper CMakeLists; pre-fix
# the lower would refuse with unsupported-source-path because
# lib/demo.c is outside cmakeSrc. Post-fix, detectWorkspaceRoot
# walks up from cmakeSrc, finds the .git marker, and uses the
# workspace root as the label base — the emitted BUILD references
# `lib/demo.c` as a workspace-relative label.
#
# Asserts:
#   1. convert-element-cmake exits 0.
#   2. The generated BUILD.bazel contains `srcs = ["lib/demo.c"]`
#      (workspace-relative path, not an absolute path or a
#      refused-source error).
#
# Cross-machine portable: runs cmake live so the recorded paths
# match the test-host paths.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

# Build the workspace tree.
mkdir -p "$work_dir/.git" "$work_dir/build/cmake" "$work_dir/lib" "$work_dir/include"
echo "ref: refs/heads/main" >"$work_dir/.git/HEAD"
cat >"$work_dir/include/demo.h" <<'EOF'
#ifndef DEMO_H
#define DEMO_H
int demo_value(void);
#endif
EOF
cat >"$work_dir/lib/demo.c" <<'EOF'
#include "demo.h"
int demo_value(void) { return 42; }
EOF
cat >"$work_dir/build/cmake/CMakeLists.txt" <<EOF
cmake_minimum_required(VERSION 3.20)
project(zstd_shape LANGUAGES C)
get_filename_component(WS "\${CMAKE_CURRENT_SOURCE_DIR}/../.." ABSOLUTE)
add_library(libdemo STATIC \${WS}/lib/demo.c)
target_include_directories(libdemo PUBLIC \${WS}/include)
EOF

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$work_dir/build/cmake" \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero on zstd-shape layout"
    echo "   workspace-root detection should have allowed lib/demo.c"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

if ! grep -q '"lib/demo.c"' "$out_build"; then
    echo "FAIL: lib/demo.c not in srcs as workspace-relative path"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi

echo "ok  meta-cmake-workspace-root: lib/demo.c emitted as workspace-relative label"
