#!/bin/sh
# meta-cmake-protoc-grpc-recognize.sh — render gate for the codegen-recognizer
# registry's gRPC-service recognizer, behind the opt-in --recognize-codegen flag.
#
# The fixture has a standalone COMBINED protoc custom command (one invocation,
# both plugins):
#   add_custom_command(OUTPUT svc.pb.{cc,h} svc.grpc.pb.{cc,h}
#       COMMAND protoc --cpp_out=<b> --grpc_out=<b>
#                      --plugin=protoc-gen-grpc=grpc_cpp_plugin -I <s> svc.proto)
#
# Default (no flag): a generic genrule (control half — the recognizer is
# provably additive + off-by-default).
#
# With --recognize-codegen: the grpc recognizer (registered ahead of the cpp
# one) claims the combined command and lowers it to the full native set —
# proto_library(svc_proto) + cc_proto_library(svc_cc_proto) +
# cc_grpc_library(svc_cc_grpc, grpc_only = True) — with the @protobuf + @grpc
# loads auto-emitted. The recognizer is the OUTPUT AUTHORITY: it derives
# svc.pb.{cc,h} + svc.grpc.pb.{cc,h} and cross-checks cmake's recorded outputs.
#
# Render-only: a bazel build of cc_grpc_library pulls the whole @grpc toolchain
# (abseil + protobuf + …), too heavy/flaky for a gate; the cc_proto build half
# is covered by meta-cmake-protoc-recognize.sh, and cc_grpc_library is grpc's
# own tested rule. The render contract is what cmd/write-a owes its consumers.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/protoc-grpc-recognize"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: WITHOUT the flag, the edge stays a genrule. ---
ctrl="$work_dir/control.BUILD"
"$bin_dir/convert-element-cmake" --source-root "$fixture" --out-build "$ctrl" \
    >"$work_dir/ctrl.out" 2>"$work_dir/ctrl.err" || fail "control convert failed" "$work_dir/ctrl.err"
grep -qE '^genrule\(' "$ctrl" || fail "control (no flag) should emit a genrule" "$ctrl"
grep -qE '^cc_grpc_library\(' "$ctrl" && fail "control must NOT emit cc_grpc_library without the flag" "$ctrl"
echo "ok  meta-cmake-protoc-grpc-recognize: default emits a generic genrule (recognizer is opt-in)"

# --- Recognizer: WITH the flag, the combined command lowers to the native set. ---
build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" --source-root "$fixture" --out-build "$build" --recognize-codegen \
    >"$work_dir/conv.out" 2>"$work_dir/conv.err" || fail "convert failed" "$work_dir/conv.err"
grep -qF 'load("@grpc//bazel:cc_grpc_library.bzl", "cc_grpc_library")' "$build" || fail "cc_grpc_library load not emitted" "$build"
grep -qF 'load("@protobuf//bazel:cc_proto_library.bzl", "cc_proto_library")' "$build" || fail "cc_proto_library load not emitted" "$build"
grep -qF 'load("@protobuf//bazel:proto_library.bzl", "proto_library")' "$build" || fail "proto_library load not emitted" "$build"
grep -qF 'name = "svc_proto"' "$build" || fail "proto_library svc_proto missing" "$build"
grep -qF 'name = "svc_cc_proto"' "$build" || fail "cc_proto_library svc_cc_proto missing" "$build"
grep -qE '^cc_grpc_library\(' "$build" || fail "combined command not lowered to cc_grpc_library" "$build"
grep -qF 'name = "svc_cc_grpc"' "$build" || fail "cc_grpc_library name wrong" "$build"
grep -qF 'srcs = [":svc_proto"]' "$build" || fail "cc_grpc_library srcs should be the proto_library" "$build"
grep -qF 'deps = [":svc_cc_proto"]' "$build" || fail "cc_grpc_library deps should be the cc_proto_library" "$build"
grep -qF 'grpc_only = True' "$build" || fail "cc_grpc_library should set grpc_only = True (unquoted)" "$build"
grep -qE '^genrule\(' "$build" && fail "the protoc genrule should be GONE under --recognize-codegen" "$build"
echo "ok  meta-cmake-protoc-grpc-recognize: combined protoc lowered to proto + cc_proto + cc_grpc_library"
echo "ok  meta-cmake-protoc-grpc-recognize: ok"
