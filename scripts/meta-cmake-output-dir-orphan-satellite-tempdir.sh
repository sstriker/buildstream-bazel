#!/bin/sh
# meta-cmake-output-dir-orphan-satellite-tempdir.sh — render+build gate for the
# CROSS-BOUNDARY + TEMP-DIR-THEN-COPY compound: a nested satellite
# (project(LANGUAGES NONE)) runs a tool in a temp dir and relocates its output
# into the OUTER build tree (OUTER_BUILD/gen) via `cmake -E copy_if_different`.
#
# The orphan attribution corroborates the cross-boundary orphans across the outer
# build dirs (orphanOnDisk), but recoverTempDirToolRelocate then re-corroborated
# the same DECLARED outputs against the SATELLITE build dir only — missing the
# cross-boundary orphans → bailing to the byte-bake. The fix corroborates declared
# outputs across the outer build dirs too (fileUnderBuildRoots), so the tool
# EXTRACTS to a regenerating genrule instead of freezing the copied bytes.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts the producer
# re-runs the tool (python3 satellite/tool.py) and is NOT a write_file bake, then
# builds AND runs //:app (exit 0 == foo_value() returns 7).
#
# Gating: skips cleanly when cmake or python3 isn't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-output-dir-orphan-satellite-tempdir"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (the cross-boundary temp-dir orphan was not recovered?)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

grep -qF 'cmake-codegen-output-dir-orphan' "$build" \
    || fail "the consumed orphans were not recovered (expected the cmake-codegen-output-dir-orphan tag)"
grep -qF 'cmake-codegen-nested-cmake' "$build" \
    || fail "the cross-boundary producer is not tagged cmake-codegen-nested-cmake"
# EXTRACT (not bake): the cross-boundary declared outputs must corroborate against
# the OUTER build dir, so the temp-dir tool is recovered rather than frozen.
grep -qF 'write_file(' "$build" \
    && fail "a write_file bake leaked in — the cross-boundary declared outputs were re-corroborated against the satellite build dir only (Gap A)"
grep -qE '^[[:space:]]*cmd = "python3 satellite/tool\.py ' "$build" \
    || fail "the orphan producer does not re-run the tool (expected python3 satellite/tool.py …)"
grep -qF '"gen/foo.c"' "$build" || fail "gen/foo.c not declared by the recovered producer"
grep -qF '"gen/foo.h"' "$build" || fail "gen/foo.h not declared by the recovered producer"
if grep -q 'resolved to an empty string' "$build" || grep -q 'resolved to an empty string' "$work_dir/convert.stderr"; then
    fail "a cmake -P refusal leaked in"
fi

echo "ok  meta-cmake-output-dir-orphan-satellite-tempdir: cross-boundary temp-dir orphans EXTRACTED (declared outputs corroborated across the outer build dir, no bake)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-output-dir-orphan-satellite-tempdir: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-output-dir-orphan-satellite-tempdir: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/satellite"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/satellite/tool.py "$ws/satellite/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "output_dir_orphan_satellite_tempdir", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the extracted cross-boundary tool genrule didn't relocate the sources?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — extracted cross-boundary orphan content wrong"
    exit 1
fi

echo "ok  meta-cmake-output-dir-orphan-satellite-tempdir: //:app builds + runs from the extracted cross-boundary tool genrule"
