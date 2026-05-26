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

# Assertion (2): re-run with --dump-vars=false to exercise the
# attribution-missed dual case. With dump-vars off AND no cmake-
# 3.32 find_package-v1 event in the configureLog (cmake's
# 3.28-3.31 pin range), findPackageAttrib can't fire — the lower
# falls through to the cmake-codegen-find-package-attribution-
# missed tag so operators see the gap. The audit framework then
# surfaces a `find-package-attribution-missed` finding pointing
# at libz.so.
#
# This re-uses the same fixture/manifest as run (1); only the
# --dump-vars flag flips, and we recycle a fresh build_dir +
# out_build so the runs don't interleave artefacts.
work_dir2="$(mktemp -d)"
trap "rm -rf '$work_dir' '$work_dir2'" EXIT

build_dir2="$work_dir2/build"
out_build2="$work_dir2/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --imports-manifest "$fixture/imports.json" \
    --out-build "$out_build2" \
    --dump-vars=false \
    >"$work_dir2/cmake.stdout" 2>"$work_dir2/cmake.stderr" || {
    echo "FAIL: convert-element-cmake (--dump-vars=false) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir2/cmake.stderr"
    exit 1
}

# When cmake on PATH is >= 3.32 it natively emits the find_package-
# v1 configureLog event, which alone is enough to attribute ZLIB
# even without dump-vars — in that case //elements/zlib appears
# and no attribution-missed tag fires. The dual case (the new
# tag) only manifests when both signals are absent, i.e. cmake
# pre-3.32 AND --dump-vars=false. Detect which branch the host
# cmake exercises and assert accordingly. The cmake version
# floor for the find_package-v1 event is 3.32 per cmake's
# release notes; the orchestrator's 3.28 pin lands squarely in
# the pre-3.32 window where the dual fires.
cmake_version=$(cmake --version | head -n1 | awk '{print $3}')
cmake_major=$(echo "$cmake_version" | cut -d. -f1)
cmake_minor=$(echo "$cmake_version" | cut -d. -f2)
if [ "$cmake_major" -gt 3 ] || { [ "$cmake_major" -eq 3 ] && [ "$cmake_minor" -ge 32 ]; }; then
    # cmake >= 3.32: configureLog supplies find_package-v1
    # natively; attribution still fires, dep still resolves. The
    # attribution-missed dual can't trigger on this host. Just
    # confirm //elements/zlib still emits and skip the dual
    # assertion (we'd need a pre-3.32 host or a synthesized
    # configureLog-empty fixture to exercise it here).
    if ! grep -q '//elements/zlib' "$out_build2"; then
        echo "FAIL: //elements/zlib missing from --dump-vars=false BUILD on cmake >= 3.32"
        echo "   the find_package-v1 configureLog event should have attributed ZLIB"
        sed 's/^/   /' "$out_build2"
        exit 1
    fi
    echo "ok  meta-cmake-find-package-variable-form: //elements/zlib emitted (cmake $cmake_version: dual path exercised in unit tests, configureLog covers --dump-vars=false here)"
else
    # cmake < 3.32: no find_package-v1 event AND --dump-vars=false
    # → findPackageAttrib returns nil → attribution-missed tag
    # fires. The audit framework converts the tag into a
    # find-package-attribution-missed finding pointing at libz.so.
    if ! grep -q 'cmake-codegen-find-package-attribution-missed=libz.so' "$out_build2"; then
        echo "FAIL: cmake-codegen-find-package-attribution-missed=libz.so missing from --dump-vars=false BUILD"
        echo "   the lower should have tagged the unattributable link fragment so the audit surfaces it"
        echo "   --- generated BUILD ---"
        sed 's/^/   /' "$out_build2"
        exit 1
    fi
    echo "ok  meta-cmake-find-package-variable-form: //elements/zlib emitted (dump-vars on); attribution-missed tag emitted (dump-vars off, cmake $cmake_version)"
fi
