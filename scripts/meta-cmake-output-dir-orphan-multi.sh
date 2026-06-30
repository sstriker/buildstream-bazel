#!/bin/sh
# meta-cmake-output-dir-orphan-multi.sh — render+build gate for the MULTI-EDGE
# OUTPUT_DIR consumed-orphan shape, exercising the over-attribution guard's
# phantom-overlap case.
#
# TWO `cmake -P` edges each run a tool (gen_a.sh / gen_b.sh) that writes its
# generated sources into a DISTINCT subdir of a shared parent (gen/a and gen/b
# under gen/). The write-dir detection adds each tool's OUTPUT_DIR operand
# (gen/a, gen/b) AND its parent (gen) as candidates, so the shared `gen` parent
# would be a PHANTOM overlap firing the over-attribution guard against BOTH edges
# — declining a perfectly unambiguous attribution. The guard must compare DEFINITE
# write targets (the operands, not the speculative parent), so gen/a and gen/b
# don't contend and both edges recover.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts BOTH orphan
# producers extract to regenerating tool genrules (no refusal, no phantom
# decline), then builds AND runs //:app (exit 0 == foo_value()==7 && bar_value()==11).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-output-dir-orphan-multi"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (a phantom-overlap decline refused the multi-edge orphans?)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# Both edges recovered (NOT declined as ambiguous): each is a regenerating tool
# genrule writing into its OWN subdir.
grep -qE '^[[:space:]]*cmd = "sh gen_a\.sh \$\(RULEDIR\)/gen/a"' "$build" \
    || fail "edge A's orphan producer missing (expected cmd = \"sh gen_a.sh \$(RULEDIR)/gen/a\")"
grep -qE '^[[:space:]]*cmd = "sh gen_b\.sh \$\(RULEDIR\)/gen/b"' "$build" \
    || fail "edge B's orphan producer missing (expected cmd = \"sh gen_b.sh \$(RULEDIR)/gen/b\") — the phantom-overlap guard declined it?"
for o in '"gen/a/foo.c"' '"gen/a/foo.h"' '"gen/b/bar.c"' '"gen/b/bar.h"'; do
    grep -qF "$o" "$build" || fail "orphan $o not recovered/consumed"
done
grep -qF 'write_file(' "$build" \
    && fail "a write_file bake leaked in — the tool-driven orphans should EXTRACT, not bake"
if grep -q 'resolved to an empty string' "$build" || grep -q 'resolved to an empty string' "$work_dir/convert.stderr"; then
    fail "a cmake -P refusal leaked in (the phantom-overlap decline?)"
fi

echo "ok  meta-cmake-output-dir-orphan-multi: both sibling-subdir orphan producers recovered (no phantom-overlap decline)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-output-dir-orphan-multi: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-output-dir-orphan-multi: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/gen_a.sh "$fixture"/gen_b.sh "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "output_dir_orphan_multi", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the two extracted tool genrules didn't both produce their sources?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# bazel-bin is a SYMLINK — find -L follows it (a plain find silently returns nothing).
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — one of the two extracted orphan sources is wrong"
    exit 1
fi

echo "ok  meta-cmake-output-dir-orphan-multi: //:app builds + runs from both extracted tool genrules"
