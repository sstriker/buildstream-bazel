#!/bin/sh
# meta-cmake-superbuild-crossboundary-cmake-script-recipe.sh — render+build gate
# for the INTERSECTION of two nested-recipe shapes: a `cmake -P` recipe producer
# AND cross-boundary output placement (the generated source written UP into the
# OUTER build tree while the recipe `.cmake` lands in the nested build dir).
#
# Both axes converge on the cmake-script BAKE read: the declared output is
# OUTER-build-relative (genSrcRelToOwningBuild), but the script ran in the NESTED
# build dir. The bake must read the output from the build dir that OWNS it
# (cc.OuterBuildDirs), not the nested workDir/buildDir — otherwise (and when
# workDir == buildDir) the read missed and a frozen EMPTY file was baked.
#
# Converted with --cmake-script-bake, the gate asserts type_a.c is a registered
# producer carrying its REAL content (gen_value returns 7) with no consumer-side
# bake and no convert-time path leak, then builds AND runs //:app (exit 0).
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

fixture="$repo_root/converter/testdata/sample-projects/superbuild-crossboundary-cmake-script-recipe"
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

# The cross-boundary gen source is a registered producer with its REAL content
# (read from the OUTER build dir), not a consumer-side bake and not an empty file.
grep -q 'baked_generated_type_a_c' "$build" \
    && fail "type_a.c fell to the consumer-side build-dir bake (cross-boundary cmake -P read missed)"
grep -qF 'generated/type_a.c' "$build" \
    || fail "type_a.c not present as a recovered output"
grep -qF 'int gen_value(void) { return 7; }' "$build" \
    || fail "recovered type_a.c is empty / wrong content — the bake read the wrong build dir"
grep -qF '/tmp/' "$build" \
    && fail "a convert-time absolute path leaked into the BUILD file"
printf '%s\n' "$(awk '/name = "app"/{f=1} f{print} /\]/{if(f)f=0}' "$build")" \
    | grep -qF '"generated/type_a.c"' \
    || fail "app srcs do not reference the recovered cross-boundary generated source"

echo "ok  meta-cmake-superbuild-crossboundary-cmake-script-recipe: cross-boundary cmake -P recipe gen source recovered (real content from owning build dir, no bake/leak)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-superbuild-crossboundary-cmake-script-recipe: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-superbuild-crossboundary-cmake-script-recipe: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sbccsr", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.7.1")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered cross-boundary cmake -P gen source didn't materialize?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered cross-boundary gen source content wrong"
    exit 1
fi

echo "ok  meta-cmake-superbuild-crossboundary-cmake-script-recipe: //:app builds + runs from the recovered cross-boundary cmake -P gen source"
