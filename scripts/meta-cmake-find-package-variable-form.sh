#!/bin/sh
# meta-cmake-find-package-variable-form.sh — render gate for the
# find_package variable-form attribution fix.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/find-package-variable-form,
# which uses the variable-form idiom (target_link_libraries
# consumes ${ZLIB_LIBRARIES} instead of the namespaced
# ZLIB::ZLIB target). Pre-fix (round-3 of PR #227, commit
# 5fccb3d), the ZLIB dep dropped silently. Post-fix, the lower's
# findPackageAttrib routes the resolved abs path back to the
# imports manifest's ZLIB::ZLIB entry and emits //elements/zlib.
#
# Asserts:
#   1. convert-element-cmake exits 0.
#   2. The generated BUILD.bazel contains `//elements/zlib` in
#      the cc_library's deps — i.e. the dep didn't silently
#      drop.
#
# cmake-availability gating: skips cleanly when no cmake >= 3.24
# is on PATH (the architectural floor for the dump-vars hook the
# attribution path consumes).

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

fixture="$repo_root/converter/testdata/sample-projects/find-package-variable-form"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build_dir="$work_dir/build"
out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --imports-manifest "$fixture/imports.json" \
    --out-build "$out_build" \
    --probe-genex \
    >"$work_dir/cmake.stdout" 2>"$work_dir/cmake.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/cmake.stderr"
    exit 1
}

if ! grep -q '//elements/zlib' "$out_build"; then
    echo "FAIL: //elements/zlib missing from generated BUILD.bazel"
    echo "   variable-form find_package attribution didn't fire — ZLIB dep dropped"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# Also confirm the round-1 audit isn't flagging anything that
# would mean the attribution succeeded by accident (e.g. a
# fallback tag instead of a real dep).
if grep -q 'cmake-codegen-find-package-fallback' "$out_build"; then
    echo "FAIL: emitted fallback tag instead of real label"
    echo "   the manifest's ZLIB::ZLIB lookup should have hit"
    sed 's/^/   /' "$out_build"
    exit 1
fi

echo "ok  meta-cmake-find-package-variable-form: //elements/zlib emitted via variable-form attribution"
