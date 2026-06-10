#!/bin/sh
# meta-cmake-defer-execute-process.sh — parity gate: execute_process lifts
# must be EQUAL whether the call is direct, cmake_language(DEFER CALL …)'d,
# or DEFER DIRECTORY'd to another scope.
#
# cmake's trace reports a deferred call's EXECUTION with the registration
# site's file/line and execution-time-expanded argv, so the trace-driven
# execute_process recovery sees the same shape either way: the
# file-producing hoist (OUTPUT_FILE → genrule) lifts identically for all
# three forms, provenance comments point at the registration site, and the
# relative-OUTPUT_FILE conservative refusal is byte-identical too (probed
# during the DEFER audit). This fixture pins the parity: three `cat tpl >
# OUTPUT_FILE` calls — direct, deferred, and DEFER DIRECTORY — whose
# consumer #includes all three generated headers.

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

fixture="$repo_root/converter/testdata/sample-projects/defer-execute-process"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$work_dir/BUILD.bazel" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

build="$work_dir/BUILD.bazel"

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# All three forms lift to hoisted genrules, identically shaped.
for out in reg.h def.h dirdef.h; do
    grep -qF "outs = [\"$out\"]" "$build" || fail "hoisted genrule for $out missing (DEFER parity regressed)"
done
[ "$(grep -c 'cmake-codegen-execute-process-hoisted' "$build")" -eq 3 ] \
    || fail "expected exactly 3 hoisted execute_process genrules"
grep -q "unsupported-" "$work_dir/convert.stderr" \
    && fail "converter emitted a typed refusal (all three forms should lift)"

echo "ok  meta-cmake-defer-execute-process: direct, DEFER, and DEFER DIRECTORY execute_process hoists lift identically"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-defer-execute-process: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-defer-execute-process: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp -r "$fixture/." "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "deferep", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //...) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: consumer of the three hoisted headers failed to build"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-defer-execute-process: the consumer compiles against all three hoisted headers (no cmake)"
