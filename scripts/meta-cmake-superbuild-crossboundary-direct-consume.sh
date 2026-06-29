#!/bin/sh
# meta-cmake-superbuild-crossboundary-direct-consume.sh — render+build gate for
# cross-boundary nested codegen consumed DIRECTLY by the outer (no recipe tie).
#
# A nested cmake sub-project whose ONLY target is a UTILITY (add_custom_target)
# runs a tool that writes its generated SOURCE UP into the OUTER build tree; the
# OUTER consumes it directly (add_executable srcs), with no `.cmake` recipe /
# include() / target_sources() (that variant is the -cmake-script-recipe gate).
#
# The nested lowering walks the sub-project's ninja custom command, but the
# output lands in the OUTER build dir. Without cross-boundary output anchoring
# the standalone recovery leaks the absolute convert-time path and the OUTER
# bakes a frozen duplicate. The fix resolves the escaping output to its OWNING
# (outer) build-relative form, so the custom command becomes a REGENERATING
# genrule the outer consumes — extract over bake.
#
# Converted with --recognize-codegen --cmake-script-trace --fidelity best-effort.
# Asserts a regenerating genrule producing generated/type_a.c (no /tmp leak, no
# consumer-side bake), then builds AND runs //:app (exit 0 == gen_value()==7).
#
# Gating: skips cleanly when cmake / python3 aren't on PATH.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo "skip: python3 not on PATH"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/superbuild-crossboundary-direct-consume"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --fidelity best-effort \
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

# The cross-boundary output is a REGENERATING genrule declaring the OUTER-relative
# out, NOT a frozen bake and NOT an absolute-path leak.
grep -qF 'baked_generated_type_a_c' "$build" \
    && fail "type_a.c fell to a consumer-side bake (cross-boundary output not extracted)"
grep -qF 'outs = ["generated/type_a.c"]' "$build" \
    || fail "the cross-boundary output is not declared as the outer-relative generated/type_a.c"
grep -qE 'python3 codegen/gen.py \$\(RULEDIR\)/generated/type_a.c' "$build" \
    || fail "the regenerating tool genrule (\$(RULEDIR)-anchored) is missing"
grep -qF '/tmp/' "$build" \
    && fail "a convert-time absolute path leaked into the BUILD file"

echo "ok  meta-cmake-superbuild-crossboundary-direct-consume: nested UTILITY cross-boundary codegen extracted to a regenerating genrule (no leak, no bake)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-superbuild-crossboundary-direct-consume: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-superbuild-crossboundary-direct-consume: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/codegen"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/codegen/gen.py "$ws/codegen/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sbcdc", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered cross-boundary genrule didn't materialize the output?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered cross-boundary gen source content wrong"
    exit 1
fi

echo "ok  meta-cmake-superbuild-crossboundary-direct-consume: //:app builds + runs from the recovered cross-boundary genrule"
