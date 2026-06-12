#!/bin/sh
# meta-cmake-genclass-textual-impl.sh — render+build gate for the genclass /
# template-implementation-include idiom.
#
# A C++ header textually #includes its own implementation — glm's
# `glm/common.hpp` ends with `#include "detail/func_common.inl"`; VTK does the
# same with `.txx`; the classic "genclass" shape does it with a literal
# `#include "foo.cc"`. The impl fragment is non-self-contained (it only makes
# sense pasted into the includer's translation unit), so under Bazel it must
# land in textual_hdrs — NOT hdrs (a parse_headers / layering_check build would
# try to compile the fragment standalone and fail) and NOT srcs (it would be
# compiled as its own TU → duplicate/undefined symbols).
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/genclass-textual-impl, where shape.hpp
# textually #includes shape_impl.inl (an inline def) and shape_impl_extra.cc (a
# HEADER_FILE_ONLY .cc def). The includer is a HEADER, so the detection only
# fires once the textual-include scan reads t.Hdrs (not just t.Srcs).
#
# Asserts (rendered BUILD):
#   1. Both impls are in textual_hdrs.
#   2. The .inl moved OUT of hdrs (only the self-contained shape.hpp stays).
#   3. The .cc is NOT in srcs (it is never compiled standalone).
# Bazel-build half (bazel >= 7): builds //:shape with parse_headers on, proving
# the impls are correctly exempted from standalone header compilation.
#
# Gating: skips cleanly when cmake isn't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/genclass-textual-impl"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- generated BUILD.bazel ---"
    sed 's/^/   /' "$out_build"
    exit 1
}

# The textual_hdrs block must carry BOTH impls.
grep -qF '"shape_impl.inl"' "$out_build" || fail "shape_impl.inl not emitted at all"
grep -qF '"shape_impl_extra.cc"' "$out_build" || fail "shape_impl_extra.cc not emitted at all"

# Extract each attribute list precisely. The start patterns are anchored on the
# 4-space attribute indent so the `hdrs`/`srcs` scans don't also match
# `textual_hdrs` (a superstring): `^    hdrs = [` vs `^    textual_hdrs = [`.
attr_block() { awk -v pat="^    $1 = \\\\[" '$0 ~ pat {f=1} f {print} /\]/ {if(f)f=0}' "$out_build"; }

textual=$(attr_block textual_hdrs)
printf '%s\n' "$textual" | grep -qF '"shape_impl.inl"' \
    || fail "shape_impl.inl is not in textual_hdrs (the .inl impl must be textual)"
printf '%s\n' "$textual" | grep -qF '"shape_impl_extra.cc"' \
    || fail "shape_impl_extra.cc is not in textual_hdrs (the .cc impl must be textual)"

# The .inl must have MOVED OUT of hdrs; the self-contained public header stays.
hdrs=$(attr_block hdrs)
printf '%s\n' "$hdrs" | grep -qF '"shape_impl.inl"' \
    && fail "shape_impl.inl is still in hdrs — it must move to textual_hdrs"
grep -qF '"shape.hpp"' "$out_build" || fail "the self-contained public header shape.hpp vanished"

# The .cc impl must NOT be compiled standalone.
srcs=$(attr_block srcs)
printf '%s\n' "$srcs" | grep -qF '"shape_impl_extra.cc"' \
    && fail "shape_impl_extra.cc is in srcs — it must not be compiled standalone"

echo "ok  meta-cmake-genclass-textual-impl: header-included .inl/.cc impls route to textual_hdrs (out of hdrs/srcs)"

# --- Bazel-build half: prove it compiles under header parsing. ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-genclass-textual-impl: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-genclass-textual-impl: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp -r "$fixture/." "$ws/"
cp "$out_build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "genclass", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# parse_headers + process_headers_in_dependencies makes Bazel compile each hdr
# standalone — which a non-self-contained .inl in hdrs would FAIL. The build
# passing proves the impls are correctly in textual_hdrs (exempt from parsing).
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} \
        --features=parse_headers --process_headers_in_dependencies \
        //:shape) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:shape under parse_headers failed (a textual impl leaked into hdrs?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-genclass-textual-impl: //:shape compiles under parse_headers (impls correctly exempt as textual_hdrs)"
