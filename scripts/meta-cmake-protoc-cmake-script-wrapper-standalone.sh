#!/bin/sh
# meta-cmake-protoc-cmake-script-wrapper-standalone.sh — render+build gate for
# recognize-THROUGH-script on the STANDALONE custom-command path (the
# per-target path's recoverCmakeScriptCodegen, ported to lowerStandaloneCustom-
# Commands via tryStandaloneCmakeScriptCodegen).
#
# The fixture's protoc output is driven by an add_custom_TARGET (no cc_library
# consumes foo.pb.cc as a source), so the ninja CUSTOM_COMMAND edge is NOT
# claimed by the per-target recoverGenrule path — it reaches the standalone
# path. With --recognize-codegen --cmake-script-trace the standalone path now
# re-traces the `cmake -P gen_proto.cmake` script, recognizes the protoc, and
# lowers it to proto_library + cc_proto_library. The re-trace runs protoc at
# convert time (skips cleanly without protoc).
#
# Gating: --recognize-codegen WITHOUT --cmake-script-trace must NOT recognize
# through the script (no proto_library), proving the recursion stays gated.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }
command -v protoc >/dev/null 2>&1 || { echo "skip: protoc not on PATH (the re-trace runs it)"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/protoc-cmake-script-wrapper-standalone"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Gating: --recognize-codegen alone (no trace) must NOT recognize through
# the script (the recursion is gated on --cmake-script-trace). ---
gate="$work_dir/gate"; mkdir -p "$gate"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$gate/BUILD.bazel" --recognize-codegen --ignore-rejections-for-diagnostics \
    >"$gate/out" 2>"$gate/err" || true
if grep -qE '^proto_library\(' "$gate/BUILD.bazel" 2>/dev/null; then
    fail "the standalone script recursion must be gated on --cmake-script-trace, but --recognize-codegen alone recognized it" "$gate/BUILD.bazel"
fi
echo "ok  meta-cmake-protoc-cmake-script-wrapper-standalone: recursion gated on --cmake-script-trace (recognize-codegen alone does not recognize)"

# --- Recover: re-trace the script, recognize the protoc, emit the native rules. ---
rec="$work_dir/rec"; mkdir -p "$rec"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$rec/BUILD.bazel" --recognize-codegen --cmake-script-trace \
    >"$rec/out" 2>"$rec/err" || fail "convert (--recognize-codegen --cmake-script-trace) failed" "$rec/err"
b="$rec/BUILD.bazel"
grep -qE '^proto_library\(' "$b" || fail "expected proto_library from the standalone script recovery" "$b"
[ "$(grep -cE '^cc_proto_library\(' "$b")" = "1" ] || fail "expected exactly one cc_proto_library (no double-emit)" "$b"
if grep -qE '^genrule\(' "$b" && grep -qF 'cmake -P' "$b"; then
    fail "no cmake -P genrule should remain (the native rule owns the protoc)" "$b"
fi
echo "ok  meta-cmake-protoc-cmake-script-wrapper-standalone: standalone cmake -P script recovered -> proto_library + cc_proto_library"

# --- Bazel-build half: the recovered cc_proto_library builds. ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-protoc-cmake-script-wrapper-standalone: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-protoc-cmake-script-wrapper-standalone: bazel < 7, skipping build half"; exit 0; fi

# The cc_proto_library target name the recognizer derives from foo.proto.
target=$(grep -oE 'name = "[a-zA-Z0-9_]*cc_proto"' "$b" | head -1 | sed -E 's/name = "(.*)"/\1/')
[ -n "$target" ] || fail "could not find the cc_proto_library target name" "$b"
ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/foo.proto" "$ws/"
cp "$b" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "protoccmakescriptstandalone", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} "//:$target" ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:$target failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-protoc-cmake-script-wrapper-standalone: //:$target builds from the standalone script-recovered cc_proto_library"
