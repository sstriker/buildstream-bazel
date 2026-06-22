#!/bin/sh
# meta-cmake-superbuild-cmake-script-recipe.sh — render+build gate for the nested
# UTILITY recipe recovery when the recipe is produced by a `cmake -P` SCRIPT.
#
# The fixture's outer configure builds a nested cmake project whose UTILITY runs
# `cmake -P gen.cmake`, which file(WRITE)s the generated source (type_a.c) AND
# the recipe (recipe.cmake) that target_sources()'s it onto the outer `app`.
# Because the recipe edge's command is cmake-script mode, the recovery diverts to
# the cmake-script path — which previously dropped the recipe's declared
# generated sources (they weren't threaded through), so type_a.c fell to the
# consumer-side build-dir bake (unregistered, per-consumer).
#
# Under --cmake-script-bake the recovery now honors the declared gen sources:
# the script runs at CONVERT time and type_a.c is emitted as a recipe-attributed
# producer (write_file) registered so the consumer wires to it — no cmake -P at
# Bazel BUILD time, no consumer-side bake.
#
# Asserts (rendered BUILD): no consumer-side `baked_*type_a*` bake; app wires the
# recovered source. Bazel-build half (bazel >= 7): //:app builds AND RUNS (exit 0
# == gen_value() returns 7).
#
# Gating: skips cleanly when cmake isn't on PATH (the bake also needs cmake at
# convert time, which this gate already requires).

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

fixture="$repo_root/converter/testdata/sample-projects/superbuild-cmake-script-recipe"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --cmake-script-bake=true \
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

# The generated source is a recovered producer, NOT a consumer-side build-dir
# bake (the bug: the cmake-script path dropped the recipe's declared gen sources).
grep -q 'baked_codegen_build_gen_type_a_c' "$build" \
    && fail "type_a.c fell to the consumer-side build-dir bake — declaredOuts not threaded through the cmake-script recovery"
grep -qF 'codegen-build/gen/type_a.c' "$build" \
    || fail "type_a.c not present as a recovered output"
# app compiles the recovered generated source.
printf '%s\n' "$(awk '/name = "app"/{f=1} f{print} /\]/{if(f)f=0}' "$build")" \
    | grep -qF '"codegen-build/gen/type_a.c"' \
    || fail "app srcs do not reference the recovered generated source"

echo "ok  meta-cmake-superbuild-cmake-script-recipe: cmake -P recipe's gen source recovered as a registered producer (no consumer-side bake)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-superbuild-cmake-script-recipe: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-superbuild-cmake-script-recipe: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sbcsr", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.7.1")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered cmake-script recipe gen source didn't materialize?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered gen source content wrong"
    exit 1
fi

echo "ok  meta-cmake-superbuild-cmake-script-recipe: //:app builds + runs from the recovered cmake -P recipe gen source"
