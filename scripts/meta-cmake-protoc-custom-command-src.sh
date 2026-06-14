#!/bin/sh
# meta-cmake-protoc-custom-command-src.sh — render+build gate for codegen
# recognition on the per-target ninja recoverGenrule path: a protoc
# add_custom_command whose generated foo.pb.cc is COMPILED as a source of a
# cc_library (so it's recovered per-target by recoverGenrule, not by the
# standalone custom-command pass).
#
# Control (no flag): recoverGenrule emits a genrule producing foo.pb.cc, which
# use_foo lists as a generated src.
# Recognizer (--recognize-codegen): protoc lowers to proto_library +
# cc_proto_library through the shared sink; rewriteNativeRuleConsumers strips
# foo.pb.cc from use_foo and wires a direct //:foo_cc_proto deps edge. Then
# bazel-builds //:use_foo.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/protoc-custom-command-src"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: per-target recoverGenrule emits a genrule. ---
ctrl="$work_dir/ctrl"; mkdir -p "$ctrl"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$ctrl/BUILD.bazel" --split-packages \
    >"$ctrl/out" 2>"$ctrl/err" || fail "control convert failed" "$ctrl/err"
grep -qE '^genrule\(' "$ctrl/BUILD.bazel" || fail "control should recover the consumed protoc as a genrule" "$ctrl/BUILD.bazel"
grep -qE '^cc_proto_library\(' "$ctrl/BUILD.bazel" && fail "control must NOT emit cc_proto_library without the flag" "$ctrl/BUILD.bazel"
echo "ok  meta-cmake-protoc-custom-command-src: control recovers the consumed protoc add_custom_command as a genrule"

# --- Recognizer: native pair + consumer rewrite, single emission. ---
rec="$work_dir/rec"; mkdir -p "$rec"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$rec/BUILD.bazel" --recognize-codegen --split-packages \
    >"$rec/out" 2>"$rec/err" || fail "recognizer convert failed" "$rec/err"
b="$rec/BUILD.bazel"
[ "$(grep -cE '^cc_proto_library\(' "$b")" = "1" ] || fail "expected exactly one cc_proto_library (no double-emit)" "$b"
grep -qF '"//:foo_cc_proto"' "$b" || fail "consumer not wired to //:foo_cc_proto" "$b"
grep -qE '^genrule\(' "$b" && fail "the protoc genrule should be GONE under the flag" "$b"
grep -qF 'foo.pb.cc' "$b" && fail "generated foo.pb.cc should be stripped from the consumer (the native rule compiles it)" "$b"
echo "ok  meta-cmake-protoc-custom-command-src: consumed protoc recognized via recoverGenrule -> cc_proto_library + direct deps (single emit, src stripped)"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-protoc-custom-command-src: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-protoc-custom-command-src: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/foo.proto" "$fixture/use_foo.cc" "$ws/"
cp "$b" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "protoccmdsrc", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:use_foo ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:use_foo failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-protoc-custom-command-src: //:use_foo builds from the recoverGenrule-recognized cc_proto_library"
