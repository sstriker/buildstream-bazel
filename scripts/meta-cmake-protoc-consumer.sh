#!/bin/sh
# meta-cmake-protoc-consumer.sh — render+build gate for the codegen-recognizer
# CONSUMER-dep generalization, behind the opt-in --recognize-codegen flag.
#
# The fixture extends protoc-recognize with a consumer: use_foo.cc #includes
# the generated foo.pb.h and the library add_dependencies() the codegen target.
# This is the --split-packages path, where the consumer-dep wiring lives.
#
# Default (no flag): protoc → genrule producing foo.pb.{cc,h}, and the consumer
# resolves the #include through the file-oriented generated_includes
# textual_hdrs wrapper (//:generated_includes). The control half asserts that.
#
# With --recognize-codegen: protoc → proto_library + cc_proto_library, and the
# consumer gets a DIRECT deps edge to the native rule (//:foo_cc_proto) — NOT
# the generated_includes wrapper, which is not synthesized at all (the header
# is produced inside the cc_proto_library). Generalized via the native-rule
# substrate (NativeRuleConsumerLabels), so it works for any recognized rule.
#
# Asserts both renders, then bazel-builds the consumer (//:use_foo) — proving
# the deps edge supplies foo.pb.h at compile time.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/protoc-consumer"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control half: WITHOUT the flag, the consumer rides the file wrapper. ---
ctrl_dir="$work_dir/ctrl"
mkdir -p "$ctrl_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$ctrl_dir/BUILD.bazel" \
    --split-packages \
    >"$work_dir/ctrl.stdout" 2>"$work_dir/ctrl.stderr" \
    || fail "control convert exited non-zero" "$work_dir/ctrl.stderr"
ctrl="$ctrl_dir/BUILD.bazel"
grep -qE '^genrule\(' "$ctrl" || fail "control should emit a genrule for protoc" "$ctrl"
grep -qF '"//:generated_includes"' "$ctrl" || fail "control consumer should dep on the generated_includes wrapper" "$ctrl"
grep -qE '^cc_proto_library\(' "$ctrl" && fail "control must NOT emit cc_proto_library without the flag" "$ctrl"
echo "ok  meta-cmake-protoc-consumer: default wires the consumer through generated_includes (recognizer opt-in)"

# --- Recognizer half: WITH the flag, the consumer deps directly on the rule. ---
rec_dir="$work_dir/rec"
mkdir -p "$rec_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$rec_dir/BUILD.bazel" \
    --split-packages \
    --recognize-codegen \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" \
    || fail "convert-element-cmake exited non-zero" "$work_dir/convert.stderr"
build="$rec_dir/BUILD.bazel"

grep -qE '^proto_library\(' "$build" || fail "protoc --cpp_out not lowered to proto_library" "$build"
grep -qE '^cc_proto_library\(' "$build" || fail "cc_proto_library not emitted" "$build"
grep -qF 'name = "foo_cc_proto"' "$build" || fail "cc_proto_library name wrong" "$build"
grep -qF '"//:foo_cc_proto"' "$build" || fail "consumer not wired with a direct deps edge to //:foo_cc_proto" "$build"
grep -qF 'generated_includes' "$build" && fail "the generated_includes wrapper must NOT be synthesized for a native-rule output" "$build"
grep -qE '^genrule\(' "$build" && fail "the protoc genrule should be GONE under --recognize-codegen" "$build"
echo "ok  meta-cmake-protoc-consumer: consumer gets a direct deps edge to //:foo_cc_proto (no file wrapper)"

# --- Bazel-build half: the consumer compiles via the native-rule dep. ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-protoc-consumer: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-protoc-consumer: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture/foo.proto" "$fixture/use_foo.cc" "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "protocconsumer", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:use_foo ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:use_foo failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-protoc-consumer: //:use_foo compiles foo.pb.h via the cc_proto_library dep"
