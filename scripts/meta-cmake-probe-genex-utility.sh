#!/bin/sh
# meta-cmake-probe-genex-utility.sh — render gate for the
# round-1 probe-genex UTILITY fix (PR #227, commit 2f67f4b).
#
# Drives convert-element-cmake with --probe-genex against
# converter/testdata/sample-projects/probe-genex-utility, which
# mixes a real STATIC_LIBRARY with an add_custom_target (UTILITY)
# and an ALIAS. Pre-fix, cmake's generation step fatal-errored
# on the UTILITY target with "Target ... is not an executable or
# library" — boost's tests / check / install-* utility targets
# would all trip this and abort conversion.
#
# Post-fix, the affirmative-type gate (EXECUTABLE /
# SHARED_LIBRARY / STATIC_LIBRARY / MODULE_LIBRARY /
# OBJECT_LIBRARY) skips UTILITY / ALIAS / INTERFACE_LIBRARY
# cleanly.
#
# Asserts:
#   1. convert-element-cmake exits 0 (no fatal in cmake's
#      generation step).
#   2. The BUILD.bazel contains a cc_library for the real
#      STATIC_LIBRARY target.
#   3. No spurious cc_library or cc_binary for the UTILITY
#      target.

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

fixture="$repo_root/converter/testdata/sample-projects/probe-genex-utility"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    --probe-genex \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero with --probe-genex"
    echo "   probe-genex.cmake gate must have crashed on UTILITY target"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

if ! grep -q 'name = "realtarget"' "$out_build"; then
    echo "FAIL: real STATIC_LIBRARY target missing from BUILD.bazel"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# UTILITY target should NOT surface as a cc_* rule. add_custom_target
# is metadata-only; the lower's existing handling routes it to a
# tag or drops it. Either way, "noisy_utility" should not appear as
# a top-level rule name.
if grep -E 'name = "noisy_utility"' "$out_build" >/dev/null; then
    echo "FAIL: UTILITY target leaked into BUILD.bazel as a rule"
    sed 's/^/   /' "$out_build"
    exit 1
fi

echo "ok  meta-cmake-probe-genex-utility: --probe-genex survives UTILITY / ALIAS / INTERFACE_LIBRARY targets"
