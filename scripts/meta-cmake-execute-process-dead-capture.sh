#!/bin/sh
# meta-cmake-execute-process-dead-capture.sh — render+build gate for the
# dead-capture analysis + the file-producing keyword upgrades.
#
# The fixture exercises three historically-refused shapes:
#   1. a SILENCED nested cmake configure (OUTPUT_VARIABLE/ERROR_VARIABLE
#      captured purely to quiet the child; never read) — the capture
#      refused the entire nested lift, and in the default strict path
#      aborted the WHOLE conversion;
#   2. a file-producing execute_process carrying TIMEOUT + INPUT_FILE +
#      ERROR_FILE — each keyword's presence refused the hoist;
#   3. a dead-captured stamp (`date OUTPUT_VARIABLE _unused`).
#
# The lift: pass 1 records capture-bearing refusals' variables; the warm
# non-expanded trace pass proves which are never read (reads are only
# visible verbatim); the re-lower clears those capture keywords, letting
# the nested lift, the hoist, and the stamp skip all proceed. The
# keyword upgrades lift TIMEOUT (configure watchdog, dropped),
# INPUT_FILE (stdin from a declared src), and ERROR_FILE (second
# declared out) instead of refusing.
#
# Asserts the shapes render with ZERO refusals, then bazel-builds and
# RUNS the binary — exit 0 proves the nested lib linked AND the
# stdin-driven genrule produced the right header bytes.

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

fixture="$repo_root/converter/testdata/sample-projects/execute-process-dead-capture"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$build" \
    --conversion-todos-report "$work_dir/todos.json" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (a silencing capture must not abort the conversion)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    echo "   --- convert.stderr (tail) ---"
    tail -20 "$work_dir/convert.stderr" | sed 's/^/   /'
    exit 1
}

grep -q 'proven unread' "$work_dir/convert.stderr" || fail "dead-capture analysis didn't run"
grep -q '"kind": "execute-process-refusal"' "$work_dir/todos.json" \
    && fail "refusal todos remain — a silenced capture or upgraded keyword still refuses"
grep -qF 'name = "sublib"' "$build" || fail "silenced nested cmake not lifted (capture should be proven dead)"
grep -qF 'deps = [":sublib"]' "$build" || fail "nested archive not wired to the merged target"
grep -qF '$(location greeting.h.in)' "$build" || fail "INPUT_FILE not lifted to a declared-src stdin redirect"
grep -qF '"greeting.err",' "$build" || fail "ERROR_FILE not lifted to a second declared out"
grep -qF '2> ' "$build" || fail "stderr redirect missing from the hoisted cmd"

echo "ok  meta-cmake-execute-process-dead-capture: silencing captures cleared, keywords lifted — zero refusals"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-execute-process-dead-capture: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-execute-process-dead-capture: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/sub"
cp "$fixture"/main.c "$fixture"/greeting.h.in "$ws/"
cp "$fixture"/sub/sub.c "$ws/sub/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "deadcapture", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the converted dead-capture project failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# Exit code IS the content check: 0 only when the nested lib's symbol
# links AND the stdin-driven genrule substituted @WHO@ correctly.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — nested link or stdin-genrule bytes wrong"
    exit 1
fi

echo "ok  meta-cmake-execute-process-dead-capture: converted project builds and runs clean"
