#!/bin/sh
# meta-cmake-execute-process-argv-codegen-crossboundary.sh — render+build gate for
# the CROSS-BOUNDARY argv-codegen lift: a nested satellite's configure-time
# execute_process names its output in argv (sort -o) and writes a generated source
# into the OUTER build tree (OUTER_BUILD/gen), consumed by the OUTER app.
#
# The argv-codegen lift corroborates the argv-named output on disk before claiming
# it (classifyArgvOutputs). Corroborating against the satellite's OWN build dir
# alone misses the cross-boundary output → the lift declines → the element falls to
# the not-lifted warning (refusal). The fix corroborates across the outer build
# dirs (fileUnderBuildRoots), so the tool lifts to a genrule the outer app consumes.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts foo.c lifts to a
# `sort -o $(location gen/foo.c) …` genrule (NOT refused), then builds AND runs
# //:app (exit 0 == foo_value() returns 7).
#
# Gating: skips cleanly when cmake or sort isn't on PATH.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v sort >/dev/null 2>&1; then
    echo "skip: sort not on PATH"
    exit 0
fi
# The fixture's outer CMakeLists configures the satellite with `cmake -G Ninja`,
# so the nested configure (run during convert) needs ninja on PATH.
if ! command -v ninja >/dev/null 2>&1; then
    echo "skip: ninja not on PATH"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/execute-process-argv-codegen-crossboundary"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (the cross-boundary argv-codegen output was not corroborated → declined?)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# The cross-boundary argv tool lifted to a genrule producing gen/foo.c (NOT
# declined for want of on-disk corroboration under the satellite build dir).
grep -qF '"gen/foo.c"' "$build" \
    || fail "gen/foo.c was not produced (the cross-boundary argv output was not corroborated across the outer build dir)"
grep -qE 'sort -o .*gen/foo\.c' "$build" \
    || fail "the argv-codegen tool did not lift (expected a sort -o gen/foo.c genrule)"
if grep -q 'resolved to an empty string' "$work_dir/convert.stderr"; then
    fail "a refusal leaked into stderr"
fi

echo "ok  meta-cmake-execute-process-argv-codegen-crossboundary: cross-boundary argv output corroborated across the outer build dir → lifted (not declined)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-execute-process-argv-codegen-crossboundary: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-execute-process-argv-codegen-crossboundary: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/satellite"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/satellite/foo.c.in "$ws/satellite/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "argv_codegen_crossboundary", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the lifted cross-boundary argv genrule didn't produce gen/foo.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — cross-boundary generated content wrong"
    exit 1
fi

echo "ok  meta-cmake-execute-process-argv-codegen-crossboundary: //:app builds + runs from the lifted cross-boundary argv genrule"
