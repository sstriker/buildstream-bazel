#!/bin/sh
# meta-cmake-output-dir-orphan-satellite-multiedge.sh — render+build gate for the
# MULTI-EDGE SHARED-COMPONENT over-attribution fix. A nested satellite
# (project(LANGUAGES NONE)) has TWO cmake -P edges writing under a shared
# component dir in the OUTER build tree (OUTER_BUILD/gen/comp):
#   - edge A (gen.cmake): temp-dir-then-copy TOOL codegen into a HASH subdir
#     (comp/a-h1); its copy names OUTPUT_DIR=comp/a-h1 as an operand.
#   - edge B (genb.cmake): a file(WRITE) writing comp/shared.h DIRECTLY into the
#     shared component dir.
#
# The orphan attribution's traced write set adds a dir operand's PARENT
# speculatively, so edge A's copy of OUTPUT_DIR=comp/a-h1 leaked `comp` into its
# write dirs and over-attributed comp/shared.h (edge B's). Edge A's attributed set
# then spanned comp/a-h1 + comp, the over-attribution guard fired against edge B,
# and edge A DECLINED — leaving fa.{c,h} unrecovered → a hard refusal ("custom
# command for gen/comp/a-h1/fa.c resolved to an empty string"). Constraining
# attribution to the edge's -D-named OUTPUT_DIR (comp/a-h1) drops the parent leak:
# edge A owns only its hash-subdir codegen (EXTRACTED as a regenerating tool
# genrule) and edge B owns comp/shared.h.
#
# Converted with --recognize-codegen --cmake-script-trace. Gating: skips cleanly
# when cmake or python3 isn't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-output-dir-orphan-satellite-multiedge"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero — edge A over-attributed edge B's shared.h, the guard fired, and fa.{c,h} went unrecovered (the parent-expansion leak)?"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# The refusal the bug produced must be gone.
if grep -q 'resolved to an empty string' "$build" || grep -q 'resolved to an empty string' "$work_dir/convert.stderr"; then
    fail "a cmake -P refusal leaked in — edge A's codegen was not recovered (over-attribution not fixed)"
fi

grep -qF 'cmake-codegen-output-dir-orphan' "$build" \
    || fail "the consumed orphans were not recovered (expected the cmake-codegen-output-dir-orphan tag)"
# Edge A EXTRACTS its hash-subdir codegen as a regenerating tool genrule (not a
# bake): the parent leak no longer forces the mixed set through the byte-bake.
grep -qE 'python3 satellite/tool\.py ' "$build" \
    || fail "edge A's codegen does not re-run the tool (expected python3 satellite/tool.py …) — over-attribution forced a bake/decline"
grep -qF '"gen/comp/a-h1/fa.c"' "$build" || fail "gen/comp/a-h1/fa.c not declared by the recovered producer"
grep -qF '"gen/comp/a-h1/fa.h"' "$build" || fail "gen/comp/a-h1/fa.h not declared by the recovered producer"
# Edge B owns the directly-under-component header (no longer swept into edge A).
grep -qF '"gen/comp/shared.h"' "$build" || fail "gen/comp/shared.h not declared (edge B's output)"

echo "ok  meta-cmake-output-dir-orphan-satellite-multiedge: edge A's hash-subdir codegen EXTRACTED, edge B's shared.h owned separately (no parent-expansion over-attribution)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-output-dir-orphan-satellite-multiedge: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-output-dir-orphan-satellite-multiedge: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/satellite"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/satellite/tool.py "$ws/satellite/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "output_dir_orphan_satellite_multiedge", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.7.1")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the extracted edge-A tool genrule + edge-B header didn't produce the sources?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — recovered orphan content wrong"
    exit 1
fi

echo "ok  meta-cmake-output-dir-orphan-satellite-multiedge: //:app builds + runs from the extracted edge-A tool genrule + edge-B header"
