#!/bin/sh
# meta-cmake-output-dir-orphan-tool.sh — render+build gate for the TOOL-DRIVEN
# OUTPUT_DIR consumed-orphan recovery (extract over bake).
#
# Identical wiring to meta-cmake-output-dir-orphan, EXCEPT gen.cmake produces the
# orphan sources by running a real TOOL (execute_process over gen.sh) into
# OUTPUT_DIR rather than file(WRITE)ing literal bytes. So the recovery must
# EXTRACT a REGENERATING genrule that re-runs gen.sh — higher fidelity than
# freezing the bytes (the file(WRITE) sibling, which has no tool to extract,
# correctly bakes; see meta-cmake-output-dir-orphan).
#
# The recovery pre-creates the OUTPUT_DIR (the real custom command's sibling
# `cmake -E make_directory` doesn't run in the standalone re-trace), re-traces the
# script, detects the OUTPUT_DIR from the execute_process tool argv, attributes
# the consumed orphans under it (corroborated on disk), substitutes the tool argv
# for `cmake -P`, and emits a genrule declaring gen/foo.c (+ gen/foo.h + the
# stamp) running `sh gen.sh $(RULEDIR)/gen`.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts the orphan
# producer is a TOOL genrule (NOT a write_file bake), then builds AND runs
# //:app (exit 0 == gen_value() returns 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-output-dir-orphan-tool"
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

# Recovered with the orphan-attribution facet — NOT refused.
grep -qF 'cmake-codegen-output-dir-orphan' "$build" \
    || fail "the consumed orphans were not recovered (expected the cmake-codegen-output-dir-orphan tag)"
# EXTRACT (not bake): the producer is a single genrule that RE-RUNS the tool, its
# outs LIST declaring gen/foo.c + gen/foo.h, with the OUTPUT_DIR anchored to
# $(RULEDIR). A write_file bake here would mean the extract-over-bake rung didn't
# fire. (The genrule uses the multi-line `outs = [...]` list, not write_file's
# singular `out =`.)
grep -qE '^[[:space:]]*cmd = "sh gen\.sh \$\(RULEDIR\)/gen"' "$build" \
    || fail "the orphan producer is not a regenerating tool genrule (expected cmd = \"sh gen.sh \$(RULEDIR)/gen\")"
grep -qF '"gen/foo.c"' "$build" \
    || fail "gen/foo.c not declared by the recovered orphan producer"
grep -qF '"gen/foo.h"' "$build" \
    || fail "gen/foo.h not declared by the recovered orphan producer"
grep -qF 'write_file(' "$build" \
    && fail "a write_file bake leaked in — the tool-driven orphans should EXTRACT, not bake"
# gen.sh is the tool src; the substituted-away wrapper gen.cmake must NOT linger.
grep -qF '"gen.sh"' "$build" \
    || fail "gen.sh (the extracted tool) not a genrule src"
grep -qF 'gen.cmake' "$build" \
    && fail "the substituted-away cmake -P wrapper gen.cmake must not remain a genrule src (G10)"
# app consumes the recovered generated source.
grep -qF '"gen/foo.c"' "$build" \
    || fail "app does not consume the recovered gen/foo.c"
if grep -q 'resolved to an empty string' "$build"; then
    fail "a cmake -P refusal leaked into the BUILD"
fi

echo "ok  meta-cmake-output-dir-orphan-tool: tool-driven orphans EXTRACTED to a regenerating gen.sh genrule (no bake, no refusal)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-output-dir-orphan-tool: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-output-dir-orphan-tool: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/gen.sh "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "output_dir_orphan_tool", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the extracted tool genrule didn't produce the sources?)"
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

echo "ok  meta-cmake-output-dir-orphan-tool: //:app builds + runs from the extracted tool genrule"
