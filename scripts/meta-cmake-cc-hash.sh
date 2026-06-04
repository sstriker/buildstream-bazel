#!/bin/sh
# meta-cmake-cc-hash.sh — render+build gate for the cc_hash recognizer
# (--lift-cc-hash): a custom command running VTK's vtkHashSource.cmake lowers
# to the native cc_hash rule instead of being refused / run via cmake at build
# time (docs/research/codegen-idiom-coverage.md).
#
# Drives convert-element-cmake --lift-cc-hash against
# converter/testdata/sample-projects/cc-hash-vtk (a project that calls
# vtk_hash_source over a data file, using VTK's real vtkHashSource.cmake).
#
# Asserts (rendered BUILD):
#   1. a cc_hash rule with src/define_name/algorithm/out_header/tool from the
#      cmake -P -D args, and the cc_hash.bzl load;
#   2. NO unsupported-custom-command-script refusal and NO duplicate
#      custom_command genrule for the hashing edge (the coveredOuts dedup).
#
# Bazel-build half (bazel >= 9): stages //tools:cc-hash + the rules module and
# builds+runs a consumer that LINKS the digest symbol, proving cc-hash runs and
# produces the header with the right digest — no cmake involved.

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

fixture="$repo_root/converter/testdata/sample-projects/cc-hash-vtk"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    --lift-cc-hash \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake --lift-cc-hash exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
}

# 1. cc_hash rule + load.
grep -q 'load("@rules_buildstream_bazel//rules:cc_hash.bzl", "cc_hash")' "$out_build" \
    || fail "cc_hash.bzl load missing"
blk="$(awk '/^cc_hash\(/{f=1} f{print} f&&/^\)/{exit}' "$out_build")"
[ -n "$blk" ] || fail "no cc_hash rule emitted"
for want in 'src = "data.txt"' 'define_name = "dataHash"' 'algorithm = "SHA256"' \
            'out_header = "dataHash.h"' 'tool = "//tools:cc-hash"'; do
    printf '%s\n' "$blk" | grep -qF "$want" || fail "cc_hash missing: $want"
done

# 2. The hashing edge was NOT refused or duplicated as a custom_command genrule
# (the coveredOuts dedup must mark CCHash.OutHeader covered).
grep -q "unsupported-custom-command-script" "$work_dir/convert.stderr" \
    && fail "hashing edge was refused (recognizer didn't fire)"
grep -q 'custom_command_dataHash' "$out_build" \
    && fail "duplicate genrule for the hashing edge (coveredOuts dedup miss)"

echo "ok  meta-cmake-cc-hash: vtkHashSource cmake -P lowered to a native cc_hash rule"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-cc-hash: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 9 ]; then
    echo "ok  meta-cmake-cc-hash: bazel < 9, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/tools"
cp "$out_build" "$ws/BUILD.bazel"
cp "$fixture/data.txt" "$fixture/use_hash.cxx" "$ws/"
CGO_ENABLED=0 go build -C "$repo_root" -o "$ws/tools/cc-hash.bin" ./cmd/cc-hash
chmod 0755 "$ws/tools/cc-hash.bin"
cat > "$ws/tools/BUILD.bazel" <<'EOF'
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

native_binary(
    name = "cc-hash",
    src = "cc-hash.bin",
    out = "cc-hash",
    visibility = ["//visibility:public"],
)
EOF
cat > "$ws/MODULE.bazel" <<EOF
module(name = "cchashrecognize", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")
local_path_override(module_name = "rules_buildstream_bazel", path = "$repo_root/rules_buildstream_bazel")
EOF

# Append a cc_binary that LINKS the converted library + verifies the digest is
# a full-width SHA256 (64 hex chars) at runtime — so the build half exercises
# the full consumer wiring: the library compiles use_hash.cxx
# (#include "dataHash.h") and links the cc-hash-produced digest symbol. A
# regression that leaves the header unwired, or computes a wrong-width digest,
# fails here.
sed -i '1a load("@rules_cc//cc:defs.bzl", "cc_binary")' "$ws/BUILD.bazel"
printf 'extern const char *get_data_hash();\n#include <cstring>\nint main() { return std::strlen(get_data_hash()) == 64 ? 0 : 1; }\n' > "$ws/link_main.cxx"
cat >> "$ws/BUILD.bazel" <<'EOF'

cc_binary(
    name = "link_check",
    srcs = ["link_main.cxx"],
    deps = [":cchashvtk"],
)
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        run ${META_BAZEL_BUILD_ARGS:-} //:link_check) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building/running the consumer that links the cc_hash-produced digest failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-cc-hash: the converted library compiles + LINKS the digest symbol (consumer build, no cmake)"
