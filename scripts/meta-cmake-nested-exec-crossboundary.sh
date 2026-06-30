#!/bin/sh
# meta-cmake-nested-exec-crossboundary.sh — render+build gate for CROSS-BOUNDARY
# execute_process codegen (the execute_process parity with add_custom_command /
# configure_file / file(GENERATE) cross-boundary anchoring).
#
# The fixture's sub-project, at CONFIGURE time, runs a tool (execute_process
# cmake -E copy) that writes a generated source UP into the OUTER build tree
# (OUTER_BUILD), not its own CMAKE_CURRENT_BINARY_DIR — once immediately and once
# via cmake_language(DEFER CALL …). The nested sublib consumes both cross-boundary
# sources; the OUTER app links sublib. Without the cross-boundary anchoring the
# nested lowering would (a) DROP the produced output (executeProcessAnchorOutput
# only relativized against the local build dir) and (b) refuse the nested
# consumer's reference (unsupported-source-path: the src is outside both the
# nested source and build trees). Both now re-home to the owning OuterBuildDir.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts the producers
# (regenerating cp genrules) declare gen_cross.c + gen_deferred.c and the nested
# consumer wires them, then bazel-builds AND runs //:app (exit 0 == cross_value()
# 42 + deferred_value() 99 both link).
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

fixture="$repo_root/converter/testdata/sample-projects/nested-exec-crossboundary"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (the cross-boundary execute_process was not recovered?)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# Both cross-boundary outputs recovered to regenerating cp genrules, re-homed to
# the OUTER-build-relative form (NOT under subbuild/, NOT refused).
grep -qF 'outs = ["gen_cross.c"]' "$build" \
    || fail "the immediate cross-boundary output gen_cross.c was not recovered/re-homed to the outer-relative form"
grep -qF 'outs = ["gen_deferred.c"]' "$build" \
    || fail "the DEFERred cross-boundary output gen_deferred.c was not recovered/re-homed"
grep -qF 'cmake-codegen-execute-process-op=copy' "$build" \
    || fail "the cross-boundary copy did not lift to an execute_process genrule"
# Extract over bake: the copies recover to REGENERATING cp genrules (re-run the
# tool), NOT frozen write_file / base64 byte bakes.
grep -qE '^[[:space:]]*cmd = "mkdir -p .* && cp ' "$build" \
    || fail "the cross-boundary copy is not a regenerating cp genrule (extract over bake)"
grep -qE 'write_file\(|--content-base64|captured at convert time' "$build" \
    && fail "a cross-boundary output was BAKED (write_file / base64) instead of extracted"
# The nested consumer (sublib) wires both cross-boundary sources (the
# unsupported-source-path refusal is closed).
grep -qF '"gen_cross.c"' "$build" \
    || fail "nested sublib does not consume the cross-boundary gen_cross.c"
grep -qF '"gen_deferred.c"' "$build" \
    || fail "nested sublib does not consume the cross-boundary gen_deferred.c"
if grep -q 'resolved to an empty string' "$build"; then
    fail "a refusal leaked into the BUILD"
fi
if grep -q 'unsupported-source-path' "$work_dir/convert.stderr"; then
    fail "the nested consumer's cross-boundary src still refuses (unsupported-source-path)"
fi

echo "ok  meta-cmake-nested-exec-crossboundary: immediate + deferred cross-boundary execute_process recovered (producer + nested consumer re-homed to the outer build dir)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-nested-exec-crossboundary: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-nested-exec-crossboundary: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/sub"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/sub/sub.c "$fixture"/sub/gen_cross.c.in "$fixture"/sub/gen_deferred.c.in "$ws/sub/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "nested_exec_crossboundary", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the cross-boundary producers/consumer didn't wire?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# bazel-bin is a SYMLINK — find -L follows it (a plain find silently returns nothing).
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — cross-boundary generated content wrong"
    exit 1
fi

echo "ok  meta-cmake-nested-exec-crossboundary: //:app builds + runs (cross-boundary execute_process sources link end-to-end)"
