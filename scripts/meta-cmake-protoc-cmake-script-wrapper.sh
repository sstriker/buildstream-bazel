#!/bin/sh
# meta-cmake-protoc-cmake-script-wrapper.sh — render+build gate for P2 of the
# wrapper-codegen coverage: protoc hidden inside a USER-authored `cmake -P`
# <script> wrapper, recovered by re-tracing the script at convert time.
#
# The fixture's add_custom_command runs `cmake -P gen_proto.cmake`; the real
# protoc lives in that script's execute_process, invisible to the ninja edge
# (it reads `cmake -P`) and the add_custom_command record (it names the
# wrapper, not the tool). So unlike P1 (cmake-GENERATED wrapper, real command in
# the trace record) this needs a one-level recursion: re-trace the script.
#
# Because configure does NOT run an add_custom_command (it runs at build time),
# the script's protoc only runs when the converter RE-TRACES it — which writes
# foo.pb.{cc,h} to the absolute build dir, the on-disk evidence the
# execute_process codegen recognizer corroborates against. The re-trace runs
# protoc, so this gate requires protoc at convert time (skips cleanly without).
#
# Control (no opt-in): the cmake -P custom command is an unrecoverable Tier-1
# refusal — convert exits non-zero. Gating check: --recognize-codegen WITHOUT
# --cmake-script-trace still refuses (the recursion is gated on the trace flag).
# P2 (--recognize-codegen --cmake-script-trace): the script's protoc lowers to
# proto_library + cc_proto_library; rewriteNativeRuleConsumers strips foo.pb.cc
# from use_foo and wires a //:foo_cc_proto deps edge. Then bazel-builds //:use_foo.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }
command -v protoc >/dev/null 2>&1 || { echo "skip: protoc not on PATH (the re-trace runs it)"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/protoc-cmake-script-wrapper"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: without the opt-in, the cmake -P-wrapped protoc refuses. ---
ctrl="$work_dir/ctrl"; mkdir -p "$ctrl"
if "$bin_dir/convert-element-cmake" --source-root "$fixture" \
        --out-build "$ctrl/BUILD.bazel" >"$ctrl/out" 2>"$ctrl/err"; then
    fail "control (no flags) should refuse the cmake -P custom command, but convert succeeded" "$ctrl/err"
fi
grep -qF 'cmake -P' "$ctrl/err" || fail "control refusal should name the cmake -P script" "$ctrl/err"
echo "ok  meta-cmake-protoc-cmake-script-wrapper: control refuses the cmake -P-wrapped protoc (unrecoverable without opt-in)"

# --- Gating: --recognize-codegen alone (no trace) still refuses. ---
gate="$work_dir/gate"; mkdir -p "$gate"
if "$bin_dir/convert-element-cmake" --source-root "$fixture" \
        --out-build "$gate/BUILD.bazel" --recognize-codegen >"$gate/out" 2>"$gate/err"; then
    fail "the script recursion must be gated on --cmake-script-trace, but --recognize-codegen alone recovered it" "$gate/err"
fi
echo "ok  meta-cmake-protoc-cmake-script-wrapper: the recursion is gated on --cmake-script-trace (recognize-codegen alone still refuses)"

# --- P2: re-trace the script, recognize the protoc, wire the consumer. ---
rec="$work_dir/rec"; mkdir -p "$rec"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$rec/BUILD.bazel" --recognize-codegen --cmake-script-trace --split-packages \
    >"$rec/out" 2>"$rec/err" || fail "P2 convert (--recognize-codegen --cmake-script-trace) failed" "$rec/err"
b="$rec/BUILD.bazel"
[ "$(grep -cE '^cc_proto_library\(' "$b")" = "1" ] || fail "expected exactly one cc_proto_library (no double-emit)" "$b"
grep -qE '^proto_library\(' "$b" || fail "expected proto_library" "$b"
grep -qF '"//:foo_cc_proto"' "$b" || fail "consumer use_foo not wired to //:foo_cc_proto" "$b"
grep -qE '^genrule\(' "$b" && fail "no cmake -P genrule should remain (the native rule owns the protoc)" "$b"
grep -qF 'foo.pb.cc' "$b" && fail "generated foo.pb.cc should be stripped from the consumer" "$b"
echo "ok  meta-cmake-protoc-cmake-script-wrapper: protoc inside the cmake -P script recovered -> proto_library + cc_proto_library + direct deps"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-protoc-cmake-script-wrapper: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-protoc-cmake-script-wrapper: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/foo.proto" "$fixture/use_foo.cc" "$ws/"
cp "$b" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "protoccmakescript", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:use_foo ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:use_foo failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-protoc-cmake-script-wrapper: //:use_foo builds from the script-recovered cc_proto_library"
