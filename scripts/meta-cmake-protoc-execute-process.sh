#!/bin/sh
# meta-cmake-protoc-execute-process.sh — render+build gate for codegen
# recognition on the execute_process path (configure-time `protoc --cpp_out`),
# including the operator Starlark recognizer.
#
# Unlike add_custom_command, execute_process RUNS at configure time and records
# no outputs in the argv (protoc derives foo.pb.{cc,h} from --cpp_out=DIR). So
# the recognizer acts as the OUTPUT AUTHORITY: it SUPPLIES the derived output
# set and the converter corroborates it against the files the configure's own
# protoc run already produced on disk. Because configure runs protoc, this gate
# requires protoc at convert time (skips cleanly without it).
#
# Three cases, all under --recognize-codegen:
#   1. producer (built-in protoc)  -> proto_library + cc_proto_library
#   2. consumer (built-in protoc)  -> the lib that compiles foo.pb.cc has it
#      stripped from srcs + a direct //:foo_cc_proto deps edge, and the file is
#      NOT byte-baked (the native rule compiles it).
#   3. operator (Starlark, gen_pb) -> same, for a tool with no built-in, via
#      --recognizers, proving the execute_process path covers Starlark too.
# Then bazel-builds the consumers.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }
command -v protoc >/dev/null 2>&1 || { echo "skip: protoc not on PATH (execute_process runs it at configure)"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

sp="$repo_root/converter/testdata/sample-projects"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- 1. producer (built-in protoc) ---
p="$work_dir/p"; mkdir -p "$p"
"$bin_dir/convert-element-cmake" --source-root "$sp/protoc-execute-process" \
    --out-build "$p/BUILD.bazel" --recognize-codegen \
    >"$p/out" 2>"$p/err" || fail "producer convert failed" "$p/err"
grep -qE '^cc_proto_library\(' "$p/BUILD.bazel" || fail "execute_process protoc not lowered to cc_proto_library" "$p/BUILD.bazel"
grep -qE '^genrule\(' "$p/BUILD.bazel" && fail "producer should not emit a genrule under the flag" "$p/BUILD.bazel"
echo "ok  meta-cmake-protoc-execute-process: configure-time protoc lowered to proto_library + cc_proto_library (output authority supplied + on-disk corroborated)"

# --- 2. consumer (built-in protoc): generated .pb.cc stripped, deps wired ---
c="$work_dir/c"; mkdir -p "$c"
"$bin_dir/convert-element-cmake" --source-root "$sp/protoc-execute-process-consumer" \
    --out-build "$c/BUILD.bazel" --recognize-codegen --split-packages \
    >"$c/out" 2>"$c/err" || fail "consumer convert failed" "$c/err"
grep -qF '"//:foo_cc_proto"' "$c/BUILD.bazel" || fail "consumer not wired to //:foo_cc_proto" "$c/BUILD.bazel"
grep -qF 'foo.pb.cc' "$c/BUILD.bazel" && fail "generated foo.pb.cc should be stripped (not baked / not a src)" "$c/BUILD.bazel"
grep -qF 'write_file' "$c/BUILD.bazel" && fail "generated sources must not be byte-baked when the native rule owns them" "$c/BUILD.bazel"
echo "ok  meta-cmake-protoc-execute-process: consumer's generated foo.pb.cc stripped + direct //:foo_cc_proto deps edge (no bake)"

# --- 3. operator Starlark recognizer (gen_pb, no built-in) on execute_process ---
s="$work_dir/s"; mkdir -p "$s"
sd="$sp/recognizer-starlark-execute-process"
"$bin_dir/convert-element-cmake" --source-root "$sd" \
    --out-build "$s/BUILD.bazel" --recognize-codegen --recognizers "$sd/recognizer.star" --split-packages \
    >"$s/out" 2>"$s/err" || fail "starlark execute_process convert failed" "$s/err"
grep -qE '^cc_proto_library\(' "$s/BUILD.bazel" || fail "operator Starlark recognizer did not lower gen_pb on the execute_process path" "$s/BUILD.bazel"
grep -qF '"//:foo_cc_proto"' "$s/BUILD.bazel" || fail "starlark consumer not wired to //:foo_cc_proto" "$s/BUILD.bazel"
grep -qF 'write_file' "$s/BUILD.bazel" && fail "starlark case must not byte-bake the generated sources" "$s/BUILD.bazel"
echo "ok  meta-cmake-protoc-execute-process: operator Starlark recognizer claims gen_pb on the execute_process path (no recompile)"

# --- Bazel-build half: both consumers compile via the native-rule dep ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-protoc-execute-process: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-protoc-execute-process: bazel < 7, skipping build half"; exit 0; fi

bzlcache="$work_dir/.bzcache"
build_consumer() {
	label_dir="$1"; src_fixture="$2"
	ws="$work_dir/ws_$3"; mkdir -p "$ws"
	cp "$src_fixture/foo.proto" "$src_fixture/use_foo.cc" "$ws/"
	cp "$label_dir/BUILD.bazel" "$ws/BUILD.bazel"
	cat > "$ws/MODULE.bazel" <<EOF
module(name = "m$3", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
	# shellcheck disable=SC2086
	( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
		build ${META_BAZEL_BUILD_ARGS:-} //:use_foo ) >"$ws/log" 2>&1 || { echo "FAIL: //:use_foo build ($3)"; sed 's/^/   /' "$ws/log"; exit 1; }
}
build_consumer "$c" "$sp/protoc-execute-process-consumer" builtin
build_consumer "$s" "$sd" starlark
echo "ok  meta-cmake-protoc-execute-process: //:use_foo builds from the execute_process-recognized rules (built-in + Starlark)"
