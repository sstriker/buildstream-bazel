#!/bin/sh
# meta-cmake-output-dir-orphan.sh — render+build gate for the OUTPUT_DIR
# consumed-orphan codegen recovery.
#
# The fixture's custom command runs `cmake -P gen.cmake`, whose ninja-declared
# OUTPUT is ONLY a `.cmake` stamp (manifest.cmake). The REAL generated sources
# (gen/foo.c, gen/foo.h) are an UNDECLARED side effect: gen.cmake file(WRITE)s
# them into the directory passed as `-DOUTPUT_DIR=<dir>`. The executable consumes
# gen/foo.c (and lists gen/foo.h), but cmake/ninja has no producer edge for either
# — only the stamp — so they are consumed ORPHANS. Without the OUTPUT_DIR orphan
# recovery the consumer bottoms out at the `cmake -P` refusal (Tier-1, EXIT 65) on
# gen/foo.c.
#
# The recovery re-traces the script, detects the write directory from where the
# traced file(WRITE)s land (NOT a hardcoded variable name), attributes the consumed
# orphans under it (corroborated on disk), and emits a regenerating producer
# declaring gen/foo.c (+ gen/foo.h + the stamp), registered so the consumer
# attaches instead of refusing.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts the orphan
# producer (tag cmake-codegen-output-dir-orphan) declares gen/foo.c and gen/foo.h
# (NOT a refusal), then builds AND runs //:app (exit 0 == gen_value() returns 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-output-dir-orphan"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (the OUTPUT_DIR orphan refusal was not recovered?)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# The undeclared orphan sources are recovered to regenerating producers tagged
# with the orphan-attribution facet — NOT refused.
grep -qF 'cmake-codegen-output-dir-orphan' "$build" \
    || fail "the consumed orphans were not recovered (expected the cmake-codegen-output-dir-orphan tag)"
# The orphan producers declare gen/foo.c and gen/foo.h (scope the assertion to
# the producer's `out =` line — comment-carrying copies CMakeLists comments into
# the BUILD, so a whole-file grep would false-match the fixture's prose).
grep -qF 'out = "gen/foo.c"' "$build" \
    || fail "gen/foo.c not declared by the recovered orphan producer"
grep -qF 'out = "gen/foo.h"' "$build" \
    || fail "gen/foo.h not declared by the recovered orphan producer"
# app consumes the recovered generated source.
grep -qF '"gen/foo.c"' "$build" \
    || fail "app does not consume the recovered gen/foo.c"
# Negative: the refusal stub must NOT appear (it would carry the empty-cmd code).
if grep -q 'resolved to an empty string' "$build"; then
    fail "a cmake -P refusal leaked into the BUILD"
fi

echo "ok  meta-cmake-output-dir-orphan: consumed orphans gen/foo.{c,h} recovered to regenerating producers (no refusal)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-output-dir-orphan: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-output-dir-orphan: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "output_dir_orphan", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered orphan producers didn't produce the sources?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# bazel-bin is a SYMLINK — find -L follows it (a plain find silently returns nothing).
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — recovered orphan content wrong"
    exit 1
fi

echo "ok  meta-cmake-output-dir-orphan: //:app builds + runs from the recovered orphan producers"
