#!/bin/sh
# meta-cmake-fileset-compiled-lib.sh — render+build gate for Phase 7
# slice 2: a COMPILED library that exports public headers via
# `target_sources(... FILE_SET HEADERS BASE_DIRS <d> ...)` lifts that
# base dir from the broad `includes = ["<d>"]` to the precise
# `strip_include_prefix = "<d>"` (liftCompiledLibFileSetStripIncludePrefix,
# converter/internal/lower/fileset_strip_prefix.go).
#
# Render half (cmake + ninja): live-convert the fileset-compiled-lib
# sample project and assert the compiled lib `fscl` carries
# `strip_include_prefix = "include"` and NO `includes = ["include"]`
# (the FILE_SET export dir was lifted, not left as a broad -I).
#
# Build half (bazel >= 9): the load-bearing check — strip_include_prefix
# on a target WITH srcs must still let the lib's OWN sources resolve
# their public `#include <fscl/fscl.hpp>` (via the virtual include root),
# and a consumer must resolve it transitively. A self-contained workspace
# mirroring the converted shape proves both compile; this is what
# de-risks the idiom (the converted BUILD's bazel half would need the
# full toolchain/rules workspace, so the shape is reproduced directly).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v ninja >/dev/null 2>&1; then
    echo "skip: ninja not on PATH (--source-root mode uses the Ninja generator)"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/fileset-compiled-lib"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

out_build="$work_dir/BUILD.bazel.out"
"$bin_dir/convert-element-cmake" --source-root "$fixture" --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

# Slice the fscl (compiled lib) rule block.
blk="$(awk '/name = "fscl"/{f=1} f{print} f&&/^\)/{exit}' "$out_build")"
if [ -z "$blk" ]; then
    echo "FAIL: fscl rule missing from the converted BUILD ($out_build)"
    sed 's/^/   /' "$out_build"
    exit 1
fi

printf '%s\n' "$blk" | grep -q 'strip_include_prefix = "include"' || {
    echo "FAIL: fscl missing strip_include_prefix = \"include\" (FILE_SET base dir not lifted)"
    printf '%s\n' "$blk" | sed 's/^/   /'
    exit 1
}
if printf '%s\n' "$blk" | grep -q 'includes = \["include"\]'; then
    echo "FAIL: fscl still carries includes = [\"include\"] — the broad -I should be replaced by strip_include_prefix"
    printf '%s\n' "$blk" | sed 's/^/   /'
    exit 1
fi
echo "ok  meta-cmake-fileset-compiled-lib: render OK — compiled lib's FILE_SET base dir lifted to strip_include_prefix"

# --- Build half: prove the shape compiles under real bazel. ---
if command -v bazel >/dev/null 2>&1; then
    BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-fileset-compiled-lib: bazel not on PATH, skipping build half"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "ok  meta-cmake-fileset-compiled-lib: bazel < 9, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/include/fscl" "$ws/src"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "fsclbuild", version = "0.0.1")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
printf '#pragma once\nint fscl_fn();\n' > "$ws/include/fscl/fscl.hpp"
printf '#include <fscl/fscl.hpp>\nint fscl_fn() { return 7; }\n' > "$ws/src/fscl.cpp"
printf '#include <fscl/fscl.hpp>\nint main() { return fscl_fn(); }\n' > "$ws/src/demo.cpp"
cat > "$ws/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")

# The converted shape: compiled lib exporting public headers via
# strip_include_prefix (no broad -Iinclude), plus a consumer.
cc_library(
    name = "fscl",
    srcs = ["src/fscl.cpp"],
    hdrs = ["include/fscl/fscl.hpp"],
    strip_include_prefix = "include",
)

cc_binary(
    name = "fscl_demo",
    srcs = ["src/demo.cpp"],
    deps = [":fscl"],
)
EOF

bzl_cache="$work_dir/.bazel"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bzl_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:fscl //:fscl_demo) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: strip_include_prefix shape did not compile (lib's own srcs or consumer broke)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-fileset-compiled-lib: strip_include_prefix compiled lib + consumer build clean under bazel $bazel_major"
