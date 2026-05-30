#!/bin/sh
# meta-cmake-enable-exports.sh — render gate for the ENABLE_EXPORTS
# native-intent lift.
#
# cmake's ENABLE_EXPORTS=TRUE on an executable means "export the
# dynamic symbol table so dlopen'd plugins resolve against the host"
# — implemented by adding the export-dynamic linker flag (-rdynamic
# / -Wl,--export-dynamic on GNU/Clang ld). Bazel expresses this
# natively as a linkopts entry, so the converter lifts it to
# linkopts = ["-rdynamic"] rather than only emitting a diagnostic
# tag for the operator to wire by hand.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/enable-exports (a single
# add_executable with ENABLE_EXPORTS TRUE).
#
# Asserts:
#   1. convert exits 0.
#   2. The host_app cc_binary carries linkopts = ["-rdynamic"] — the
#      native effect, not just a tag.
#   3. The cmake-codegen-enable-exports tag is retained for
#      auditability.
#   4. The bazel-idiom audit emits NO enable-exports finding (the
#      lift means there's no operator gap left to flag).
#
# cmake 3.24+ is required for the structural probe that captures the
# ENABLE_EXPORTS property; the gate skips cleanly when cmake isn't on
# PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/enable-exports"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"
idiom="$work_dir/bazel-idiom.json"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    --probe-genex=true \
    --audit-bazel-idiom-report "$idiom" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

blk="$(awk '/name = "host_app"/,/^\)/' "$out_build")"
if [ -z "$blk" ]; then
    echo "FAIL: host_app cc_binary missing from BUILD.bazel"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# 2. Native effect present.
if ! printf '%s\n' "$blk" | grep -qF -- '"-rdynamic"'; then
    echo "FAIL: host_app missing linkopts = [\"-rdynamic\"] — ENABLE_EXPORTS"
    echo "   should lift to the native export-dynamic linkopt, not just a tag"
    printf '%s\n' "$blk" | sed 's/^/   /'
    exit 1
fi

# 3. Audit tag retained.
if ! printf '%s\n' "$blk" | grep -qF -- 'cmake-codegen-enable-exports'; then
    echo "FAIL: host_app missing cmake-codegen-enable-exports tag (auditability)"
    printf '%s\n' "$blk" | sed 's/^/   /'
    exit 1
fi

# 4. No enable-exports audit finding (the gap is closed).
if [ -f "$idiom" ] && grep -q 'enable-exports-toolchain-feature-needed' "$idiom"; then
    echo "FAIL: bazel-idiom audit still emits enable-exports-toolchain-feature-needed"
    echo "   the native lift means there is no operator gap left to flag"
    sed 's/^/   /' "$idiom"
    exit 1
fi

echo "ok  meta-cmake-enable-exports: ENABLE_EXPORTS lifted to native linkopts=[\"-rdynamic\"] (tag retained for audit, no idiom finding)"
