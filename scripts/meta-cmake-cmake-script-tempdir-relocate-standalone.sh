#!/bin/sh
# meta-cmake-cmake-script-tempdir-relocate-standalone.sh — render+build gate for
# the STANDALONE cmake -P codegen path running the full recovery ladder.
#
# The fixture's codegen output is produced by an add_custom_target (no compile
# target consumes it), so it routes through lowerStandaloneCustomCommands ->
# tryStandaloneCmakeScriptCodegen rather than the per-target recoverGenrule path.
# The script has a TEMP-DIR-RELOCATE shape (tool in mktemp -d, then cmake -E copy
# to the declared output). Before the standalone path ran the full ladder it fell
# to the per-step recoverExecuteProcess and FROZE-BAKED the copy; with the ladder
# wired in (recoverTempDirToolRelocate via recoverCmakeScriptCodegen) it recovers
# the regenerating tool genrule instead.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts a regenerating
# genrule (driver=python3, the lifted cwd copy), no frozen bake, no /tmp leak,
# then builds the genrule and checks the produced value.h content. Building a
# native genrule needs no rules_cc, so the build half runs without BCR fetches.
#
# Gating: skips cleanly when cmake / python3 aren't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-tempdir-relocate-standalone"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
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

# The standalone temp-dir-relocate cmake -P is recovered to a regenerating tool
# genrule with the lifted cwd copy, NOT a frozen bake and NOT a /tmp leak.
grep -qF 'cmake-codegen-driver=python3' "$build" \
    || fail "the standalone codegen tool was not recovered via the ladder (expected a python3-driver genrule)"
grep -qF 'outs = ["gen/value.h"]' "$build" \
    || fail "gen/value.h not declared by the recovered standalone genrule"
grep -qF 'cp value.h $(RULEDIR)/gen/value.h' "$build" \
    || fail "the lifted temp-dir relocate copy (cwd output -> \$(RULEDIR)/declared) is missing"
grep -q 'cmake-codegen-execute-process-op=copy' "$build" \
    && fail "the relocate froze to a copy bake instead of the regenerating tool genrule"
# Scope the leak checks to the genrule cmd line — the carried CMakeLists comment
# legitimately mentions mktemp / the tempdir, so checking the whole file false-fails.
cmd_line=$(grep 'cmd =' "$build" || true)
printf '%s' "$cmd_line" | grep -qF '/tmp/' \
    && fail "a system-tempdir path leaked into the recovered cmd"
printf '%s' "$cmd_line" | grep -qF 'mktemp' \
    && fail "the dead mktemp side-effect stage leaked into the recovered cmd"

echo "ok  meta-cmake-cmake-script-tempdir-relocate-standalone: standalone cmake -P temp-dir-relocate recovered to a regenerating genrule (no bake, no /tmp leak)"

# --- Bazel-build half (native genrule — no rules_cc / BCR fetch needed) ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-cmake-script-tempdir-relocate-standalone: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-cmake-script-tempdir-relocate-standalone: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
# Stage BOTH declared srcs: the recovered tool genrule conservatively keeps
# gen.cmake (the cmake -P script, from the edge's DEPENDS) in srcs even though the
# lifted cmd runs python3 tool.py and never references it. Bazel requires every
# declared src to exist, so stage gen.cmake too or the genrule fails on a missing
# input.
cp "$fixture"/gen.cmake "$fixture"/tool.py "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "tdrs", version = "0.0.0")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:gen_gen_value_h) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the recovered genrule failed (the standalone ladder genrule didn't produce its output?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# bazel-bin is a symlink into the output base; find needs -L to traverse a
# symlinked START point (matching the sibling build gates, e.g.
# meta-cmake-module-library.sh). value.h is a genrule output -> under bazel-bin.
produced=$(find -L "$ws"/bazel-bin -name value.h 2>/dev/null | head -1)
if [ -z "$produced" ] || ! grep -qF '#define GEN_VALUE 7' "$produced"; then
    echo "FAIL: the regenerated value.h is missing or has the wrong content"
    exit 1
fi

echo "ok  meta-cmake-cmake-script-tempdir-relocate-standalone: the recovered genrule builds + regenerates value.h (GEN_VALUE 7)"
