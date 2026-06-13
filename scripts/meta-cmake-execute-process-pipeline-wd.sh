#!/bin/sh
# meta-cmake-execute-process-pipeline-wd.sh — render+build gate for the
# multi-COMMAND pipeline and WORKING_DIRECTORY lifts.
#
# Two historically-refused shapes:
#   1. a multi-COMMAND execute_process (cmake chains stage stdout like a
#      shell pipe, stages concurrent) with OUTPUT_FILE — refused flat as
#      "multi-COMMAND pipeline"; lifts as `( a | b ) > out`;
#   2. WORKING_DIRECTORY moved off the build root with a RELATIVE
#      OUTPUT_FILE (cmake resolves it against the cwd) — refused as
#      unmodeled; lifts with the exec-root-save prologue
#      (`_r="$$PWD" && cd <wd>`) and `$$_r/`-prefixed Bazel references,
#      the out anchored under the cwd.
#
# Asserts both lift with ZERO refusals, then bazel-builds and RUNS the
# consumer — exit 0 proves the piped bytes and the cwd-relative output
# both carry the right content.

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

fixture="$repo_root/converter/testdata/sample-projects/execute-process-pipeline-wd"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$build" \
    --conversion-todos-report "$work_dir/todos.json" \
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

grep -q '"kind": "execute-process-refusal"' "$work_dir/todos.json" \
    && fail "refusal todos remain — a pipeline or WORKING_DIRECTORY shape still refuses"
grep -qF '( cat $(location defs.txt) | sort )' "$build" || fail "pipeline not lifted as a shell pipe"
grep -qF '"work/out/num.h"' "$build" || fail "relative OUTPUT_FILE not anchored under the WORKING_DIRECTORY"
grep -qF 'cd work' "$build" || fail "WORKING_DIRECTORY prologue missing"
grep -qF '$$_r/$(location num.h.in)' "$build" || fail "Bazel reference not exec-root-prefixed under the moved cwd"

echo "ok  meta-cmake-execute-process-pipeline-wd: pipeline + WORKING_DIRECTORY lifted — zero refusals"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-execute-process-pipeline-wd: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-execute-process-pipeline-wd: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/defs.txt "$fixture"/num.h.in "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "pipewd", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the converted pipeline-wd project failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — piped or cwd-relative output bytes wrong"
    exit 1
fi

echo "ok  meta-cmake-execute-process-pipeline-wd: converted project builds and runs clean"
