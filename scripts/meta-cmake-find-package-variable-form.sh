#!/bin/sh
# meta-cmake-find-package-variable-form.sh — render gate for
# trace-driven attribution of the find_package variable-form idiom.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/find-package-variable-form,
# which uses the variable-form idiom (target_link_libraries
# consumes ${ZLIB_LIBRARIES} instead of the namespaced
# ZLIB::ZLIB target). cmake --trace-expand records the arm as the
# resolved host libz.so path; attributeDirectTraceDeps lifts it
# via systemLibName → the manifest's link_libraries:["z"] entry →
# //elements/zlib (a producer claiming the name wins over -lz).
#
# Asserts:
#   1. convert-element-cmake exits 0.
#   2. The generated BUILD.bazel contains `//elements/zlib` in the
#      cc_library's deps — i.e. the dep didn't silently drop.
#   3. No cmake-unresolved-link-arm tag fires — the manifest
#      resolved the arm, so there is no harvest gap to surface.
#
# Note: attribution is now decoupled from --dump-vars (it comes
# from the expanded target_link_libraries trace, not the
# <Pkg>_LIBRARIES cmakeVars), so this gate no longer exercises a
# dump-vars dual case.
#
# cmake-availability gating: skips cleanly when no cmake is on
# PATH. The converter's --source-root configure needs codemodel-v2
# (cmake >= 3.24); this gate doesn't re-check the version — the
# converter itself surfaces a below-floor cmake.

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
    echo "   variable-form trace attribution didn't fire — ZLIB dep dropped"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# The manifest resolved the arm to a real label, so no harvest-gap
# tag should fire (that tag is reserved for a link-line library
# the manifest can't resolve and that isn't a toolchain system lib).
if grep -q 'cmake-unresolved-link-arm' "$out_build"; then
    echo "FAIL: emitted unresolved-link-arm tag instead of resolving to a label"
    echo "   the manifest's link_libraries:[\"z\"] redirect should have hit"
    sed 's/^/   /' "$out_build"
    exit 1
fi

cmake_version=$(cmake --version | head -n1 | awk '{print $3}')
echo "ok  meta-cmake-find-package-variable-form: //elements/zlib emitted via trace-driven link_libraries redirect (cmake $cmake_version)"
