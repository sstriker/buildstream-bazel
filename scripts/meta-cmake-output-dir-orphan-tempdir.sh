#!/bin/sh
# meta-cmake-output-dir-orphan-tempdir.sh — render+build gate for the
# TEMP-DIR-THEN-COPY OUTPUT_DIR consumed-orphan recovery (extract over bake).
#
# The `cmake -P` wrapper runs a tool in a TEMP dir (WORKING_DIRECTORY=<tmp>, so
# its argv names the tempdir, NOT OUTPUT_DIR) and then file(COPY)s the tool's
# output (foo.c / foo.h) into OUTPUT_DIR. The ninja edge declares only the
# manifest stamp; foo.{c,h} are consumed orphans the app compiles.
#
# The direct-write tool extract can't see the producer (the tool's argv names the
# tempdir, not OUTPUT_DIR). The recovery must keep the script's copy relocations
# and reuse recoverTempDirToolRelocate, extracting a regenerating tool genrule
# that re-runs the tool and relocates its output to $(RULEDIR) — NOT freezing the
# copied bytes (the write_file bake the bare attribution would fall to).
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts the orphan
# producer is a regenerating tool genrule with a relocation cmd (NOT a write_file
# bake), then builds AND runs //:app (exit 0 == foo_value() returns 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-output-dir-orphan-tempdir"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (the temp-dir-then-copy orphan was not recovered?)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# Recovered with the orphan-attribution facet — NOT refused.
grep -qF 'cmake-codegen-output-dir-orphan' "$build" \
    || fail "the consumed orphans were not recovered (expected the cmake-codegen-output-dir-orphan tag)"
# EXTRACT (not bake): a regenerating genrule that RE-RUNS the tool and relocates
# its tempdir output to $(RULEDIR). A write_file bake here means the temp-dir rung
# didn't fire and the copied bytes were frozen instead.
grep -qF 'write_file(' "$build" \
    && fail "a write_file bake leaked in — the temp-dir-then-copy orphans should EXTRACT, not freeze the copied bytes"
grep -qE '^[[:space:]]*cmd = "python3 tool\.py .*cp foo\.c \$\(RULEDIR\)/gen/foo\.c' "$build" \
    || fail "the orphan producer is not a regenerating tool genrule with a relocation cmd (expected python3 tool.py … cp foo.c \$(RULEDIR)/gen/foo.c)"
grep -qF '"gen/foo.c"' "$build" \
    || fail "gen/foo.c not declared by the recovered orphan producer"
grep -qF '"gen/foo.h"' "$build" \
    || fail "gen/foo.h not declared by the recovered orphan producer"
# tool.py is the extracted tool src; the substituted-away wrapper gen.cmake must NOT linger.
grep -qF '"tool.py"' "$build" \
    || fail "tool.py (the extracted tool) not a genrule src"
grep -qF 'gen.cmake' "$build" \
    && fail "the substituted-away cmake -P wrapper gen.cmake must not remain a genrule src (G10)"
if grep -q 'resolved to an empty string' "$build" || grep -q 'resolved to an empty string' "$work_dir/convert.stderr"; then
    fail "a cmake -P refusal leaked in"
fi

echo "ok  meta-cmake-output-dir-orphan-tempdir: temp-dir-then-copy orphans EXTRACTED to a regenerating tool genrule (no bake, no refusal)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-output-dir-orphan-tempdir: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-output-dir-orphan-tempdir: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/tool.py "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "output_dir_orphan_tempdir", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the extracted temp-dir tool genrule didn't relocate the sources?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# bazel-bin is a SYMLINK — find -L follows it (a plain find silently returns nothing).
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — extracted orphan content wrong"
    exit 1
fi

echo "ok  meta-cmake-output-dir-orphan-tempdir: //:app builds + runs from the extracted temp-dir tool genrule"
