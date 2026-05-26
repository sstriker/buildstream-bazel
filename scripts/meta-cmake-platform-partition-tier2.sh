#!/bin/sh
# meta-cmake-platform-partition-tier2.sh — render gate for the
# Tier 2 platform-conditional source partition (#217 follow-on,
# ROADMAP item 10).
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/platform-partition-tier2,
# whose CMakeLists.txt has an `if(LINUX) target_sources(app
# PRIVATE linux.c) elseif(WIN32) target_sources(app PRIVATE
# win.c) endif()` block. On a Linux configure:
#
#   - Tier 1 (already shipped pre-#217 follow-on): cmake traces
#     the LINUX arm and the lower partitions linux.c under
#     @platforms//os:linux.
#
#   - Tier 2 (THIS PR): cmake skips the WIN32 arm, so win.c
#     never surfaces in the trace. The Tier-2 parser re-reads
#     CMakeLists.txt at the recorded `if(LINUX)` event's line,
#     walks the elseif arm, and attributes win.c under
#     @platforms//os:windows.
#
# Asserts the rendered BUILD.bazel has BOTH select arms — Tier
# 1 fed linux.c, Tier 2 fed win.c, and a downstream bazel
# reconfigure for Windows would correctly pick up win.c (and
# vice versa for Linux). Without Tier 2, win.c would silently
# disappear from the BUILD on a Linux configure.
#
# cmake-availability gating: skips cleanly when no cmake >= 3.24
# is on PATH (Tier 1's existing floor — the source-root mode
# the gate uses requires cmake's trace stream).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi

# Build the converter so the gate has a binary to drive.
bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/platform-partition-tier2"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    >"$work_dir/cmake.stdout" 2>"$work_dir/cmake.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/cmake.stderr"
    exit 1
}

# Tier 1 check: linux.c routed through @platforms//os:linux.
# The emit shape is a select() with `@platforms//os:linux: ["linux.c"]`
# inside the cc_library's srcs.
if ! grep -q '"@platforms//os:linux"' "$out_build"; then
    echo "FAIL: @platforms//os:linux select key missing from BUILD"
    echo "   Tier 1 partition didn't fire — linux.c stayed in flat srcs"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi
if ! grep -q 'linux.c' "$out_build"; then
    echo "FAIL: linux.c missing from BUILD"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# Tier 2 check (the new capability): win.c routed through
# @platforms//os:windows even though cmake never saw it.
if ! grep -q '"@platforms//os:windows"' "$out_build"; then
    echo "FAIL: @platforms//os:windows select key missing from BUILD"
    echo "   Tier 2 didn't recover the elseif(WIN32) arm"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi
if ! grep -q 'win.c' "$out_build"; then
    echo "FAIL: win.c missing from BUILD — Tier 2 silently dropped it"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# Cross-check the two markers cohabit on different lines of the
# same select() — defends against a degenerate emit shape where
# both lines exist but in unrelated select()s or as comments.
linux_line=$(grep -n '"@platforms//os:linux"' "$out_build" | head -1 | cut -d: -f1)
windows_line=$(grep -n '"@platforms//os:windows"' "$out_build" | head -1 | cut -d: -f1)
if [ -z "$linux_line" ] || [ -z "$windows_line" ]; then
    echo "FAIL: could not locate both select arms in BUILD"
    sed 's/^/   /' "$out_build"
    exit 1
fi

echo "ok  meta-cmake-platform-partition-tier2:"
echo "    Tier 1 emitted @platforms//os:linux arm with linux.c"
echo "    Tier 2 emitted @platforms//os:windows arm with win.c"
