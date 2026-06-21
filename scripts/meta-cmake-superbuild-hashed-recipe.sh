#!/bin/sh
# meta-cmake-superbuild-hashed-recipe.sh — render+build gate for the nested
# UTILITY recipe recovery with a per-configure-UNSTABLE recipe filename.
#
# The fixture's outer configure builds a nested cmake project whose UTILITY
# (add_custom_target) emits a codegen "recipe" .cmake whose filename carries a
# per-configure-incrementing counter (recipe-<N>.cmake), plus a generated source
# (gen_src.c, stable name) the recipe target_sources()'s into the outer `app` as
# an UNDECLARED side output. The recipe's ninja-output name from the driver's
# traced re-configure (recipe-<higher N>) therefore DIVERGES from the name in the
# outer trace (recipe-<lower N>).
#
# The recovery must: pair the recipe edge to the outer include by STABLE STEM
# (recipe-*.cmake), declare the STABLE gen_src (not the unstable recipe) on the
# recovered genrule, re-home it under subbuild/ with the command's output dir
# anchored to $(RULEDIR), and NOT leave a frozen bake or a dead recipe genrule.
#
# Asserts (rendered BUILD): gen_src wired to a genrule (no bake, no dead recipe).
# Bazel-build half (bazel >= 7): //:app builds AND RUNS (exit 0 == gen_value()
# returns 42 from the regenerated source).
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

fixture="$repo_root/converter/testdata/sample-projects/superbuild-hashed-recipe"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# gen_src recovered to a genrule (the stable output), not frozen-baked.
grep -qF 'outs = ["subbuild/gen/gen_src.c"]' "$build" \
    || fail "gen_src not declared by a recovered genrule (stem pairing / gen_src declaration regressed?)"
grep -qF 'python3 sub/gen.py $(RULEDIR)/subbuild/gen' "$build" \
    || fail "recovered genrule cmd's output dir not anchored to \$(RULEDIR)/subbuild/gen (re-home regressed?)"
grep -qF '"sub/gen.py"' "$build" \
    || fail "recovered genrule's source not anchored at the outer label root (sub/gen.py)"
grep -q 'baked_subbuild_gen_gen_src' "$build" \
    && fail "gen_src was frozen-baked — the recovery upgrade didn't fire"
grep -q 'custom_command_gen_recipe' "$build" \
    && fail "a dead recipe genrule leaked — the standalone-pass dedup regressed"
# app compiles the recovered gen_src.
printf '%s\n' "$(awk '/name = "app"/{f=1} f{print} /\]/{if(f)f=0}' "$build")" \
    | grep -qF '"subbuild/gen/gen_src.c"' \
    || fail "app srcs do not reference the recovered gen_src"

echo "ok  meta-cmake-superbuild-hashed-recipe: hash-unstable nested recipe recovered to a regenerating genrule (no bake, no dead recipe)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-superbuild-hashed-recipe: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-superbuild-hashed-recipe: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/sub"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/sub/gen.py "$ws/sub/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sbhr", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered nested-recipe genrule didn't produce gen_src?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# The binary's exit code IS the content check: it returns 0 only when the
# recovered genrule regenerated gen_src.c with gen_value() returning 42.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered gen_src content wrong"
    exit 1
fi

echo "ok  meta-cmake-superbuild-hashed-recipe: //:app builds + runs from the recovered hash-unstable-recipe genrule"
