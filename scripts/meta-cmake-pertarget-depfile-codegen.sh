#!/bin/sh
# meta-cmake-pertarget-depfile-codegen.sh — render+build gate for the per-target
# genrule recovery stripping ninja DEPFILE plumbing (transform parity with the
# standalone path).
#
# gen.c is consumed as a source of `app`, so it routes through the per-target
# recovery (recoverGenrule -> emitRecoveredGenrule). Its custom command declares
# a DEPFILE, so cmake's Ninja generator appends `&& cmake -E cmake_transform_depfile
# Ninja gccdepfile …` referencing the HOST cmake (absent on the Bazel executor)
# with absolute build paths. Until the depfile-plumbing strip was wired into the
# per-target path, the recovered genrule kept that unrunnable segment.
#
# Asserts the recovered genrule cmd carries NO cmake_transform_depfile segment and
# no convert-time /tmp leak, then builds + runs //:app (gen_value() == 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-pertarget-depfile-codegen"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
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

# The recovered genrule produces gen.c with the depfile plumbing stripped.
grep -qF 'outs = ["gen.c"]' "$build" \
    || fail "gen.c not declared by the recovered per-target genrule"
# Scope to the genrule cmd line — the carried CMakeLists comment legitimately
# mentions cmake_transform_depfile, so checking the whole file false-fails.
cmd_line=$(grep 'cmd =' "$build" || true)
printf '%s' "$cmd_line" | grep -qF 'cmake_transform_depfile' \
    && fail "the ninja depfile plumbing (cmake -E cmake_transform_depfile, host cmake) was not stripped from the cmd"
printf '%s' "$cmd_line" | grep -qF '/tmp/' \
    && fail "a convert-time absolute path leaked into the recovered cmd"

echo "ok  meta-cmake-pertarget-depfile-codegen: per-target genrule strips the ninja depfile plumbing (no host-cmake transform, no /tmp leak)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-pertarget-depfile-codegen: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-pertarget-depfile-codegen: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/tool.py "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "ptdc", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered genrule with stripped depfile plumbing didn't produce gen.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered gen.c content wrong"
    exit 1
fi

echo "ok  meta-cmake-pertarget-depfile-codegen: //:app builds + runs from the recovered per-target genrule"
