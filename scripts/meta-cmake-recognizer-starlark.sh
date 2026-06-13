#!/bin/sh
# meta-cmake-recognizer-starlark.sh — gate for OPERATOR codegen recognizers
# written in Starlark and loaded via --recognizers, without recompiling the
# converter.
#
# The fixture's codegen tool is `gen_pb`, a project-specific protobuf compiler
# wrapper the converter has NO built-in recognizer for. The fixture ships a
# recognizer.star that maps `gen_pb --cpp_out` to proto_library +
# cc_proto_library. Because gen_pb is not a built-in, the operator script is
# what fires (it isn't shadowed) — proving the no-recompile extension path.
#
# Control (flag on, recognizers NOT loaded): gen_pb stays a genrule and the
# consumer rides the generated_includes wrapper.
# Operator (--recognizers <star>): the script lowers gen_pb to the native rule
# pair and the consumer gets a direct //:foo_cc_proto deps edge.
#
# Then bazel-builds the consumer (//:use_foo) to prove the script-emitted rules
# are real.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/recognizer-starlark"
star="$fixture/recognizer.star"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: recognizers NOT loaded → gen_pb stays a genrule. ---
ctrl_dir="$work_dir/ctrl"; mkdir -p "$ctrl_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" --out-build "$ctrl_dir/BUILD.bazel" \
    --recognize-codegen --split-packages \
    >"$work_dir/ctrl.out" 2>"$work_dir/ctrl.err" \
    || fail "control convert exited non-zero" "$work_dir/ctrl.err"
grep -qE '^genrule\(' "$ctrl_dir/BUILD.bazel" || fail "control (no --recognizers) should emit a genrule for gen_pb" "$ctrl_dir/BUILD.bazel"
grep -qE '^cc_proto_library\(' "$ctrl_dir/BUILD.bazel" && fail "control must NOT emit cc_proto_library without the operator recognizer" "$ctrl_dir/BUILD.bazel"
echo "ok  meta-cmake-recognizer-starlark: gen_pb stays a genrule without the operator recognizer (built-ins don't claim it)"

# --- Also assert the canonical template compiles/loads (loaded on a fixture
#     the built-in protoc recognizer handles, so a load error would surface). ---
tmpl_dir="$work_dir/tmpl"; mkdir -p "$tmpl_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" --out-build "$tmpl_dir/BUILD.bazel" \
    --recognize-codegen --recognizers "$repo_root/recognizers/protoc.star" --split-packages \
    >"$work_dir/tmpl.out" 2>"$work_dir/tmpl.err" \
    || fail "loading recognizers/protoc.star failed to compile" "$work_dir/tmpl.err"
echo "ok  meta-cmake-recognizer-starlark: recognizers/protoc.star template compiles + loads"

# --- Operator: load the gen_pb recognizer → native rule pair + direct dep. ---
rec_dir="$work_dir/rec"; mkdir -p "$rec_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" --out-build "$rec_dir/BUILD.bazel" \
    --recognize-codegen --recognizers "$star" --split-packages \
    >"$work_dir/rec.out" 2>"$work_dir/rec.err" \
    || fail "operator-recognizer convert exited non-zero" "$work_dir/rec.err"
build="$rec_dir/BUILD.bazel"
grep -qE '^proto_library\(' "$build" || fail "gen_pb not lowered to proto_library by the operator recognizer" "$build"
grep -qE '^cc_proto_library\(' "$build" || fail "cc_proto_library not emitted" "$build"
grep -qF '"//:foo_cc_proto"' "$build" || fail "consumer not wired with a direct deps edge to //:foo_cc_proto" "$build"
grep -qF 'generated_includes' "$build" && fail "generated_includes wrapper must NOT be synthesized for a native-rule output" "$build"
grep -qE '^genrule\(' "$build" && fail "the gen_pb genrule should be GONE once the operator recognizer claims it" "$build"
echo "ok  meta-cmake-recognizer-starlark: operator Starlark recognizer lowered gen_pb to proto_library + cc_proto_library (no recompile)"

# --- Bazel-build half: the script-emitted rules are real. ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-recognizer-starlark: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-recognizer-starlark: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/foo.proto" "$fixture/use_foo.cc" "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "recognizerstarlark", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:use_foo ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:use_foo failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-recognizer-starlark: //:use_foo builds from the operator-recognizer's rules"
