#!/bin/sh
# meta-cc-embed-recognize.sh — render+build gate for the cc_embed
# recognizer (--lift-cc-embed): a custom command running VTK's
# vtkEncodeString.cmake encoder lowers to the native cc_embed rule
# instead of being refused / run via cmake at build time
# (docs/research/codegen-idiom-coverage.md).
#
# Drives convert-element-cmake --lift-cc-embed against
# converter/testdata/sample-projects/cc-embed-vtk (a project that calls
# vtk_encode_string over a .glsl, using VTK's real vtkEncodeString.cmake).
#
# Asserts (rendered BUILD):
#   1. a cc_embed rule with src/symbol/out_header/out_source/tool from the
#      cmake -P -D args, and the cc_embed.bzl load;
#   2. NO unsupported-custom-command-script refusal and NO duplicate
#      genrule for the encoder edge.
#
# Bazel-build half (bazel >= 9): stages //tools:cc-embed + the rules
# module and builds the cc_embed target, proving it runs cc-embed and
# produces the .h + .cxx with the embedded symbol — no cmake involved.

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

fixture="$repo_root/converter/testdata/sample-projects/cc-embed-vtk"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    --lift-cc-embed \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake --lift-cc-embed exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
}

# 1. cc_embed rule + load.
grep -q 'load("@rules_buildstream_bazel//rules:cc_embed.bzl", "cc_embed")' "$out_build" \
    || fail "cc_embed.bzl load missing"
blk="$(awk '/^cc_embed\(/{f=1} f{print} f&&/^\)/{exit}' "$out_build")"
[ -n "$blk" ] || fail "no cc_embed rule emitted"
for want in 'src = "shader.glsl"' 'symbol = "shader_glsl"' 'out_header = "shader_glsl.h"' \
            'out_source = "shader_glsl.cxx"' 'tool = "//tools:cc-embed"'; do
    printf '%s\n' "$blk" | grep -qF "$want" || fail "cc_embed missing: $want"
done

# 2. The encoder edge was NOT refused or duplicated as a genrule.
grep -q "unsupported-custom-command-script" "$work_dir/convert.stderr" \
    && fail "encoder edge was refused (recognizer didn't fire)"
grep -q 'custom_command_shader' "$out_build" \
    && fail "duplicate genrule for the encoder edge (dedup miss)"

echo "ok  meta-cc-embed-recognize: vtkEncodeString cmake -P lowered to a native cc_embed rule"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cc-embed-recognize: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 9 ]; then
    echo "ok  meta-cc-embed-recognize: bazel < 9, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/tools"
cp "$out_build" "$ws/BUILD.bazel"
cp "$fixture/shader.glsl" "$fixture/use_shader.cxx" "$ws/"
CGO_ENABLED=0 go build -C "$repo_root" -o "$ws/tools/cc-embed.bin" ./cmd/cc-embed
chmod 0755 "$ws/tools/cc-embed.bin"
cat > "$ws/tools/BUILD.bazel" <<'EOF'
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

native_binary(
    name = "cc-embed",
    src = "cc-embed.bin",
    out = "cc-embed",
    visibility = ["//visibility:public"],
)
EOF
cat > "$ws/MODULE.bazel" <<EOF
module(name = "ccembedrecognize", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")
local_path_override(module_name = "rules_buildstream_bazel", path = "$repo_root/rules_buildstream_bazel")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:gen_shader_glsl_h) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: bazel build of the recognized cc_embed failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
gen_src="$(find -L "$ws/bazel-bin" -name 'shader_glsl.cxx' 2>/dev/null | head -1)"
if [ -z "$gen_src" ] || ! grep -q "shader_glsl" "$gen_src"; then
    echo "FAIL: cc_embed didn't produce shader_glsl.cxx defining the symbol"
    exit 1
fi

echo "ok  meta-cc-embed-recognize: cc_embed builds (runs cc-embed, emits the embedded symbol) — no cmake at build time"
