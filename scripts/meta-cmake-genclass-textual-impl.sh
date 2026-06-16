#!/bin/sh
# meta-cmake-genclass-textual-impl.sh — render+build gate for the genclass /
# template-implementation-include idiom, split across the ZERO-READ default and
# the read-based --detect-fused-sources opt-in.
#
# A C++ header textually #includes its own implementation — glm's
# `glm/common.hpp` ends with `#include "detail/func_common.inl"`; VTK does the
# same with `.txx`; the classic "genclass" shape does it with a literal
# `#include "foo.cc"`. The impl fragment is non-self-contained (it only makes
# sense pasted into the includer's translation unit), so under Bazel it must
# land in textual_hdrs — NOT hdrs (a parse_headers / layering_check build would
# try to compile the fragment standalone and fail) and NOT srcs.
#
# The fixture (converter/testdata/sample-projects/genclass-textual-impl) has
# shape.hpp textually #include BOTH shape_impl.inl (an impl-header extension)
# and shape_impl_extra.cc (a HEADER_FILE_ONLY .cc). These exercise the two tiers:
#   - shape_impl.inl: routed to textual_hdrs by EXTENSION, with NO file read,
#     in the DEFAULT path (the common glm/VTK genclass form). This is the
#     performance-preserving zero-read tier.
#   - shape_impl_extra.cc: a .cc textually #included by a header but not listed
#     as a compiled source — only the read-based scan can find it, so it needs
#     the opt-in --detect-fused-sources (which reads source bytes). OFF by
#     default so the common convert reads zero files; an emitted note points the
#     operator at the flag.
#
# Bazel-build half (bazel >= 7) uses the --detect-fused-sources BUILD (both impls
# staged) and builds //:shape under parse_headers, proving the impls are exempt
# from standalone header compilation.
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

fail() {
    echo "FAIL: $1"
    echo "   --- generated BUILD.bazel ($2) ---"
    sed 's/^/   /' "$2"
    exit 1
}

# Extract one attribute's list block precisely. Anchored on the 4-space
# attribute indent so `hdrs`/`srcs` don't also match `textual_hdrs`.
attr_block() { awk -v pat="^    $1 = \\\\[" '$0 ~ pat {f=1} f {print} /\]/ {if(f)f=0}' "$2"; }

# --- (1) DEFAULT (zero-read): the .inl routes by extension; the .cc does not. ---
def_build="$work_dir/default.BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$def_build" \
    >"$work_dir/def.stdout" 2>"$work_dir/def.stderr" || {
    echo "FAIL: convert (default) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/def.stderr"
    exit 1
}
# The .inl is in textual_hdrs and out of hdrs (zero-read extension routing).
printf '%s\n' "$(attr_block textual_hdrs "$def_build")" | grep -qF '"shape_impl.inl"' \
    || fail "default: shape_impl.inl must route to textual_hdrs by extension (zero read)" "$def_build"
printf '%s\n' "$(attr_block hdrs "$def_build")" | grep -qF '"shape_impl.inl"' \
    && fail "default: shape_impl.inl must move OUT of hdrs" "$def_build"
grep -qF '"shape.hpp"' "$def_build" || fail "default: the self-contained public header shape.hpp vanished" "$def_build"
# The .cc impl is NOT detected by default (needs the read-based opt-in).
grep -qF '"shape_impl_extra.cc"' "$def_build" \
    && fail "default: shape_impl_extra.cc must NOT be detected zero-read (it is the read-based fused case)" "$def_build"
# The opt-in note is emitted so the operator knows to enable the flag.
grep -qiF 'detect-fused-sources' "$work_dir/def.stderr" \
    || { echo "FAIL: default convert should emit the --detect-fused-sources note"; sed 's/^/   /' "$work_dir/def.stderr"; exit 1; }
echo "ok  meta-cmake-genclass-textual-impl: default routes the .inl impl to textual_hdrs by extension with no file read (+ opt-in note)"

# --- (2) --detect-fused-sources (read-based): BOTH impls route. ---
fused_build="$work_dir/fused.BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --detect-fused-sources \
    --out-build "$fused_build" \
    >"$work_dir/fused.stdout" 2>"$work_dir/fused.stderr" || {
    echo "FAIL: convert (--detect-fused-sources) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/fused.stderr"
    exit 1
}
textual=$(attr_block textual_hdrs "$fused_build")
printf '%s\n' "$textual" | grep -qF '"shape_impl.inl"' \
    || fail "fused: shape_impl.inl not in textual_hdrs" "$fused_build"
printf '%s\n' "$textual" | grep -qF '"shape_impl_extra.cc"' \
    || fail "fused: shape_impl_extra.cc not in textual_hdrs (the read-based scan should find it)" "$fused_build"
printf '%s\n' "$(attr_block srcs "$fused_build")" | grep -qF '"shape_impl_extra.cc"' \
    && fail "fused: shape_impl_extra.cc must not be compiled standalone" "$fused_build"
echo "ok  meta-cmake-genclass-textual-impl: --detect-fused-sources additionally routes the header-#included .cc impl to textual_hdrs"

# --- Bazel-build half: build //:shape from the fused BUILD (both impls staged). ---
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
cp "$fused_build" "$ws/BUILD.bazel"
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
