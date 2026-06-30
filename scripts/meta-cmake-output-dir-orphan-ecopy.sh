#!/bin/sh
# meta-cmake-output-dir-orphan-ecopy.sh — render+build gate for the OUTPUT_DIR
# consumed-orphan recovery when the temp-dir relocation is a SUBPROCESS copy
# (execute_process(COMMAND cmake -E copy_if_different <srcs…> <OUTPUT_DIR>)),
# not a cmake-native file(COPY).
#
# The `cmake -E copy_if_different … ${OUTPUT_DIR}` call names the orphans' dir as
# its destination, so the direct-write tool scan would mis-pick the RELOCATION as
# the generator and emit a broken genrule (`cp _relocate_tmp/foo.c … $(RULEDIR)/gen`)
# that copies a tempdir file nothing regenerates. The recovery must treat the
# `cmake -E copy` subprocess as a relocation and recover the real python tool
# behind it — a regenerating genrule that runs the tool and relocates its output
# to $(RULEDIR).
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts the orphan
# producer RE-RUNS the tool (python3 tool.py) and is NOT the broken copy-only
# genrule, then builds AND runs //:app (exit 0 == foo_value() returns 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-output-dir-orphan-ecopy"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (the subprocess-copy orphan was not recovered?)"
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
# The producer must RE-RUN THE TOOL (python3), then relocate its tempdir output to
# $(RULEDIR). A cmd that only copies (no python3) means the direct-write scan
# mis-picked the `cmake -E copy` relocation as the generator — the broken shape.
grep -qE '^[[:space:]]*cmd = "python3 tool\.py ' "$build" \
    || fail "the orphan producer does not re-run the tool (the cmake -E copy relocation was mis-picked as the generator?)"
grep -q '_relocate_tmp' "$build" \
    && fail "the producer copies from the tempdir (a broken genrule copying a file nothing regenerates)"
grep -qF 'write_file(' "$build" \
    && fail "a write_file bake leaked in — the subprocess-copy orphans should EXTRACT, not freeze the copied bytes"
grep -qF '"gen/foo.c"' "$build" \
    || fail "gen/foo.c not declared by the recovered orphan producer"
grep -qF '"gen/foo.h"' "$build" \
    || fail "gen/foo.h not declared by the recovered orphan producer"
grep -qF '"tool.py"' "$build" \
    || fail "tool.py (the extracted tool) not a genrule src"
grep -qF 'gen.cmake' "$build" \
    && fail "the substituted-away cmake -P wrapper gen.cmake must not remain a genrule src (G10)"
if grep -q 'resolved to an empty string' "$build" || grep -q 'resolved to an empty string' "$work_dir/convert.stderr"; then
    fail "a cmake -P refusal leaked in"
fi

echo "ok  meta-cmake-output-dir-orphan-ecopy: subprocess-copy (cmake -E) orphans EXTRACTED to a regenerating tool genrule (relocation not mis-picked, no bake)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-output-dir-orphan-ecopy: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-output-dir-orphan-ecopy: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/tool.py "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "output_dir_orphan_ecopy", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the extracted tool genrule didn't relocate the sources?)"
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

echo "ok  meta-cmake-output-dir-orphan-ecopy: //:app builds + runs from the extracted tool genrule"
