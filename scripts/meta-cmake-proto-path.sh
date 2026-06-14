#!/bin/sh
# meta-cmake-proto-path.sh — render+build gate for codegen recognition under a
# rebased `--proto_path` (the proto lives under a sub-dir, and protos import each
# other by names relative to that root, not the source root).
#
# This is the shape the cross-package gate's source-root layout doesn't cover:
#   protoc --proto_path=<src>/proto --cpp_out=<bin> <src>/proto/main.proto
#   main.proto: import "dep.proto";   (relative to proto/, not the source root)
#
# The recognizer recovers the proto_path root from the proto src vs its
# canonical (output-derived) name, so it: places the rules in //proto (where the
# .proto lives, so basename srcs resolve), sets strip_import_prefix=/proto (so
# the import path stays proto_path-relative), and resolves `import "dep.proto"`
# to //proto:dep_proto. Then bazel-builds //proto:main_cc_proto.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/proto-proto-path"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

out="$work_dir/out"; mkdir -p "$out"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$out/BUILD.bazel" --recognize-codegen --split-packages \
    >"$work_dir/convert.out" 2>"$work_dir/convert.err" || fail "convert failed" "$work_dir/convert.err"

b="$out/proto/BUILD.bazel"
[ -f "$b" ] || fail "rules not placed in the //proto proto_path-root package" "$work_dir/convert.err"
grep -qF 'strip_import_prefix = "/proto"' "$b" || fail "proto_library missing strip_import_prefix=/proto for the rebased proto_path" "$b"
grep -qF 'srcs = ["dep.proto"]' "$b" || fail "dep_proto srcs should be the proto_path-relative basename" "$b"
grep -qF '"//proto:dep_proto"' "$b" || fail "main_proto missing the resolved import dep //proto:dep_proto" "$b"
echo "ok  meta-cmake-proto-path: rebased --proto_path → //proto placement + strip_import_prefix + import dep resolved"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-proto-path: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-proto-path: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws/proto"
cp "$fixture/proto/dep.proto" "$fixture/proto/main.proto" "$ws/proto/"
cp "$b" "$ws/proto/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "protopath", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //proto:main_cc_proto ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //proto:main_cc_proto failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-proto-path: //proto:main_cc_proto builds (rebased import resolves via strip_import_prefix + deps)"
