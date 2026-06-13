#!/bin/sh
# meta-cmake-protoc-recognize.sh — render+build gate for the codegen-recognizer
# registry's protoc --cpp_out recognizer, behind the opt-in --recognize-codegen
# flag.
#
# The fixture has a standalone protoc custom command (nothing consumes the
# generated sources):
#   add_custom_command(OUTPUT foo.pb.cc foo.pb.h
#       COMMAND protoc --cpp_out=<build> -I <src> foo.proto)
#
# Default (no flag): the standalone custom-command path emits a generic genrule
# — the control half asserts that, so the recognizer is provably ADDITIVE and
# off-by-default.
#
# With --recognize-codegen: the registry recognizes protoc --cpp_out by its
# DRIVER + flags and lowers it to the idiomatic native rule pair —
# proto_library(foo_proto) + cc_proto_library(foo_cc_proto) — with the
# @protobuf//bazel loads auto-emitted by the native-rule machinery. The
# recognizer is the OUTPUT AUTHORITY: it derives foo.pb.{cc,h} from the .proto
# basename and cross-checks against cmake's recorded outputs.
#
# Asserts the native rules render, then bazel-builds the cc_proto_library.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/protoc-recognize"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control half: WITHOUT the flag, the edge stays a genrule. ---
ctrl="$work_dir/control.BUILD"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$ctrl" \
    >"$work_dir/ctrl.stdout" 2>"$work_dir/ctrl.stderr" \
    || fail "control convert exited non-zero" "$work_dir/ctrl.stderr"
# Anchor rule-call assertions at column 0: emitted rules start there, while the
# carried CMake comment (which names proto_library/cc_proto_library) is
# #-prefixed, so it can't false-match.
grep -qE '^genrule\(' "$ctrl" || fail "control (no --recognize-codegen) should emit a genrule" "$ctrl"
grep -qE '^proto_library\(' "$ctrl" && fail "control must NOT emit proto_library without the flag" "$ctrl"
echo "ok  meta-cmake-protoc-recognize: default emits a generic genrule (recognizer is opt-in)"

# --- Recognizer half: WITH the flag, the edge lowers to the native pair. ---
build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$build" \
    --recognize-codegen \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" \
    || fail "convert-element-cmake exited non-zero" "$work_dir/convert.stderr"

grep -qF 'load("@protobuf//bazel:proto_library.bzl", "proto_library")' "$build" || fail "proto_library load not emitted" "$build"
grep -qF 'load("@protobuf//bazel:cc_proto_library.bzl", "cc_proto_library")' "$build" || fail "cc_proto_library load not emitted" "$build"
grep -qE '^proto_library\(' "$build" || fail "protoc --cpp_out not lowered to proto_library" "$build"
grep -qF 'name = "foo_proto"' "$build" || fail "proto_library name wrong" "$build"
grep -qF 'srcs = ["foo.proto"]' "$build" || fail "proto_library srcs not the .proto" "$build"
grep -qE '^cc_proto_library\(' "$build" || fail "cc_proto_library not emitted" "$build"
grep -qF 'name = "foo_cc_proto"' "$build" || fail "cc_proto_library name wrong" "$build"
grep -qF 'deps = [":foo_proto"]' "$build" || fail "cc_proto_library deps not the proto_library" "$build"
grep -qE '^genrule\(' "$build" && fail "the protoc genrule should be GONE under --recognize-codegen" "$build"
echo "ok  meta-cmake-protoc-recognize: protoc --cpp_out lowered to proto_library + cc_proto_library"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-protoc-recognize: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-protoc-recognize: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture/foo.proto" "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "protocrecognize", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:foo_cc_proto ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:foo_cc_proto failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-protoc-recognize: cc_proto_library builds via @protobuf"
