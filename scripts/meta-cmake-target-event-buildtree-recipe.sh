#!/bin/sh
# meta-cmake-target-event-buildtree-recipe.sh — render+build gate for the
# source-tree relaxation of TARGET-event command extraction.
#
# A PRE_LINK stamp add_custom_command is declared inside a recipe .cmake that is
# generated UNDER THE BUILD TREE and include()d — so the command's defining file
# is outside the source root. A stamp hook is output-bearing (here it generates
# gen_impl.c via a `>` redirect), and recipes/cmake-modules don't run under Bazel,
# so the output must be reproduced. The (relaxed, no longer inSourceTree-gated)
# extractor must therefore still capture the command — on the pre-relaxation code
# it was dropped and the consumer's gen_impl.c dangled.
#
# Asserts (rendered BUILD):
#   1. A target-event genrule (cmake-codegen-target-event-command) produces gen_impl.c
#      even though the command is defined in a build-tree recipe.
#   2. The consumer target's srcs reference gen_impl.c (resolved, not dropped).
# Bazel-build half (bazel >= 7): //:consumer builds — proving the genrule produced
# by the build-tree-recipe stamp actually generates gen_impl.c.
#
# Gating: skips cleanly when cmake isn't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/target-event-buildtree-recipe"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --cmake-define BUILD_SHARED_LIBS=OFF \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- generated BUILD.bazel ---"
    sed 's/^/   /' "$out_build"
    exit 1
}

attr_block() { awk -v pat="$1" '$0 ~ pat {f=1} f {print} /\]/ {if(f)f=0}' "$out_build"; }

# 1. The build-tree-recipe stamp command was captured and recovered as a genrule.
grep -qF 'cmake-codegen-target-event-command' "$out_build" \
    || fail "no target-event genrule emitted for the stamp defined in a build-tree recipe (inSourceTree relaxation regressed?)"
grep -qE 'outs = \["gen_impl\.c"\]' "$out_build" \
    || fail "the stamp byproduct gen_impl.c is not a genrule output"

# 2. The consumer compiles the recovered output (resolved, not dropped).
printf '%s\n' "$(attr_block '^    name = "consumer"')" | grep -qF '"gen_impl.c"' \
    || fail "consumer srcs do not reference the recovered gen_impl.c"

echo "ok  meta-cmake-target-event-buildtree-recipe: stamp in a build-tree recipe captured + recovered as a genrule + consumer resolves it"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-target-event-buildtree-recipe: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-target-event-buildtree-recipe: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp -r "$fixture/." "$ws/"
cp "$out_build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "tgteventrecipe", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:consumer) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:consumer failed (the build-tree-recipe stamp genrule didn't produce gen_impl.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-target-event-buildtree-recipe: //:consumer builds from the build-tree-recipe stamp genrule"
