#!/bin/sh
# meta-cmake-probe-genex-object-library.sh — render gate for the
# probe-genex OBJECT_LIBRARY path (ROADMAP "$<TARGET_OBJECTS:t>
# for OBJECT_LIBRARY targets" — Later → Done).
#
# Drives convert-element-cmake with --probe-genex against
# converter/testdata/sample-projects/object-library, which has
# an OBJECT_LIBRARY (`objlib_obj`) and a consumer STATIC_LIBRARY
# (`objlib_archive`) inlining the objects via
# $<TARGET_OBJECTS:objlib_obj>.
#
# Asserts:
#   1. convert-element-cmake exits 0 (probe-genex.cmake's
#      OBJECT_LIBRARY branch emits cleanly — pre-fix it tried to
#      emit $<TARGET_FILE:objlib_obj> too, which cmake fatal-errors
#      on for OBJECT_LIBRARY targets).
#   2. The BUILD.bazel contains a cc_library for the OBJECT_LIBRARY
#      target (the lifter currently models OBJECT_LIBRARY as
#      cc_library + alwayslink=True per converter/internal/lower/lower.go).
#   3. The BUILD.bazel contains a cc_library for the consumer
#      STATIC_LIBRARY.
#
# The probe-genex hook's objects.txt emission is the load-bearing
# wire: it captures cmake's authoritative $<TARGET_OBJECTS:t>
# resolution at generation time so internal/genexeval's (a) lift
# can answer the genex offline without needing cmake to re-run.
# The Go-side TestProbeGenex_ObjectLibrary_LiveCMake test pins
# the file emission shape; this gate exercises the end-to-end
# converter flow.

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

fixture="$repo_root/converter/testdata/sample-projects/object-library"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    --probe-genex \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero with --probe-genex on OBJECT_LIBRARY"
    echo "   probe-genex.cmake's OBJECT_LIBRARY branch likely tried to emit \$<TARGET_FILE:>"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

if ! grep -q 'name = "objlib_obj"' "$out_build"; then
    echo "FAIL: OBJECT_LIBRARY target objlib_obj missing from BUILD.bazel"
    sed 's/^/   /' "$out_build"
    exit 1
fi

if ! grep -q 'name = "objlib_archive"' "$out_build"; then
    echo "FAIL: consumer STATIC_LIBRARY objlib_archive missing from BUILD.bazel"
    sed 's/^/   /' "$out_build"
    exit 1
fi

echo "ok  meta-cmake-probe-genex-object-library: --probe-genex survives OBJECT_LIBRARY targets end-to-end"
