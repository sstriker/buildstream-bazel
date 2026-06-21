#!/bin/sh
# meta-cmake-superbuild-crossboundary-recipe.sh — render+build gate for the
# nested UTILITY recipe recovery with CROSS-BOUNDARY output placement.
#
# The fixture's outer configure builds a nested cmake project whose UTILITY
# (add_custom_target) emits a recipe .cmake into the NESTED build dir, but writes
# the generated source (type_a.c) UP into the OUTER build tree (passed as
# OUTER_GEN_DIR). The recipe's target_sources() adds that outer-tree source to
# the outer `app`. Because the generated source ESCAPES the nested build dir, the
# recovery used to drop it (it relativized against the nested build dir only) and
# the outer pass byte-baked it as a write_file.
#
# The recovery must instead resolve the escaping source against the OUTER build
# dir that owns it, declare it on a regenerating genrule with the cmd's
# outer-build output path reanchored to $(RULEDIR), and leave NO frozen bake.
#
# Asserts (rendered BUILD): type_a.c wired to a genrule, cmd reanchored to
# $(RULEDIR), no convert-time absolute path leak, no write_file bake.
# Bazel-build half (bazel >= 7): //:app builds AND RUNS (exit 0 == gen_value()
# returns 7 from the regenerated source).
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

fixture="$repo_root/converter/testdata/sample-projects/superbuild-crossboundary-recipe"
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

# The outer-tree generated source is recovered to a regenerating genrule (the
# escaping source, captured against the OUTER build dir), not frozen-baked.
grep -qF 'outs = ["generated/type_a.c"]' "$build" \
    || fail "type_a.c not declared by a recovered genrule (cross-boundary capture regressed?)"
grep -qF 'python3 codegen/gen.py $(RULEDIR)/generated' "$build" \
    || fail "recovered genrule cmd's outer-build output path not reanchored to \$(RULEDIR)/generated"
grep -q 'baked_generated_type_a_c' "$build" \
    && fail "type_a.c was frozen-baked — the cross-boundary recovery didn't fire"
grep -qF '/tmp/convert-element-build-' "$build" \
    && fail "a convert-time absolute build path leaked into the BUILD file"
# app compiles the recovered generated source.
printf '%s\n' "$(awk '/name = "app"/{f=1} f{print} /\]/{if(f)f=0}' "$build")" \
    | grep -qF '"generated/type_a.c"' \
    || fail "app srcs do not reference the recovered cross-boundary generated source"

echo "ok  meta-cmake-superbuild-crossboundary-recipe: outer-tree generated source recovered to a regenerating genrule (no bake, cmd reanchored to \$(RULEDIR))"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-superbuild-crossboundary-recipe: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-superbuild-crossboundary-recipe: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/codegen"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/codegen/gen.py "$ws/codegen/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sbcr", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered cross-boundary genrule didn't produce type_a.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# The binary's exit code IS the content check: it returns 0 only when the
# recovered genrule regenerated type_a.c with gen_value() returning 7.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered cross-boundary source content wrong"
    exit 1
fi

echo "ok  meta-cmake-superbuild-crossboundary-recipe: //:app builds + runs from the recovered cross-boundary genrule"
