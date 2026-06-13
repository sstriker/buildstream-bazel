#!/bin/sh
# meta-cmake-execute-process-cmake-e-wrappers.sh — render+build gate for
# the cmake -E wrapper normalization + console/exit-status benign skips.
#
# `cmake -E env K=V <cmd>` and `cmake -E chdir <dir> <cmd>` are
# MODIFIERS around an inner command — the same semantics execute_process
# spells as ENVIRONMENT / WORKING_DIRECTORY — and `cmake -E cat / echo /
# <algo>sum` are portable POSIX spellings. All historically refused as
# "not in the supported op set". The normalization unwraps the wrappers
# into the call's fields and rewrites the POSIX-equivalents to raw argv,
# so the inner command classifies on its own merits; console-only and
# exit-status forms (echo, true, compare_files) skip benignly.
#
# Asserts zero refusals + the env-prefixed and concat genrules, then
# bazel-builds and RUNS the consumer (exit 0 = both generated headers
# carry the right values).

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

fixture="$repo_root/converter/testdata/sample-projects/execute-process-cmake-e-wrappers"
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
    && fail "refusal todos remain — a wrapper or console form still refuses"
grep -qF 'env SUFFIX=gen sed' "$build" || fail "cmake -E env wrapper not unwrapped into an env-prefixed hoist"
grep -qF 'cat $(location part1.h.in) $(location part2.h.in)' "$build" || fail "cmake -E cat not lifted as a concat genrule"

echo "ok  meta-cmake-execute-process-cmake-e-wrappers: wrappers unwrapped, POSIX-equivalents lifted, console forms skip — zero refusals"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-execute-process-cmake-e-wrappers: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-execute-process-cmake-e-wrappers: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/val.h.in "$fixture"/part1.h.in "$fixture"/part2.h.in "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "ewrap", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the converted cmake-e-wrappers project failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — env-prefixed or concat output bytes wrong"
    exit 1
fi

echo "ok  meta-cmake-execute-process-cmake-e-wrappers: converted project builds and runs clean"
