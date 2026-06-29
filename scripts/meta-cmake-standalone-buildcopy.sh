#!/bin/sh
# meta-cmake-standalone-buildcopy.sh — render+build gate for the build-dir
# staging-copy reanchor (reanchorBuildDirCopyGenrule) on the STANDALONE genrule
# path, in parity with the per-target emitRecoveredGenrule path.
#
# The grpc shape: cmake copies a source-tree input into a build-dir staging dir
# at CONFIGURE time and runs a codegen tool cd'd INTO that dir
# (`cd <build>/staged && tool -I . data/x.txt`), so `-I .` and `data/x.txt` are
# cwd-relative reads of the staged copy. Here the OUTPUT is wrapped by an
# add_custom_target with no compile-source consumer, so the edge routes to
# lowerStandaloneCustomCommands (NOT the per-target recovery). gen.py is a
# stand-in for protoc (a faithful protoc fixture would drag in the protobuf BCR
# toolchain); it has the same cd-into-staging-dir, `-I . <rel>` shape.
#
# Before the wiring, the standalone genrule kept the cwd-relative `-I .` and bare
# `data/x.txt` reads (which point at the empty Bazel exec-root cwd) and carried
# the producerless `staged/data/x.txt` copy as a dangling input. The gate asserts
# the recovered genrule cmd reads the SOURCE-TREE-anchored input
# (`elements/sbc/data/x.txt`, `-I elements/sbc`) and that the staged copy was
# dropped, then builds the genrule + checks gen.c content.
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-standalone-buildcopy"
pkg="elements/sbc"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --bazel-package-path "$pkg" \
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

grep -qF 'outs = ["gens/x.gen.c"]' "$build" \
    || fail "gens/x.gen.c not declared by the recovered standalone genrule"

# Scope every negative/positive read assertion to the genrule cmd line — the
# carried CMakeLists comment legitimately mentions `-I .` etc., so checking the
# whole file false-fails.
cmd_line=$(grep 'cmd =' "$build" || true)
[ -n "$cmd_line" ] || fail "no genrule cmd line in the rendered BUILD"

# AFTER the reanchor: the include root + the input read are SOURCE-TREE-anchored.
printf '%s' "$cmd_line" | grep -qF -- "$pkg/data/x.txt" \
    || fail "the input read was not re-anchored to the source tree ($pkg/data/x.txt)"
printf '%s' "$cmd_line" | grep -qF -- "-I $pkg" \
    || fail "the -I include root was not re-anchored to the package ($pkg)"
# NEGATIVE: the bare cwd-relative reads must be gone from the cmd.
printf '%s' "$cmd_line" | grep -qE -- '(^| )-I \. ' \
    && fail "the cwd-relative '-I .' read survived (points at the empty exec-root cwd)"
printf '%s' "$cmd_line" | grep -qE -- ' data/x\.txt( |$)' \
    && fail "a bare cwd-relative 'data/x.txt' read survived (unanchored)"

# The producerless build-dir staging copy must be dropped from srcs.
grep -qF 'staged/data/x.txt' "$build" \
    && fail "the producerless build-dir staging copy (staged/data/x.txt) was not dropped from srcs"

echo "ok  meta-cmake-standalone-buildcopy: standalone genrule re-anchors the build-dir staging copy to the source tree (no '-I .', no bare cwd read, copy dropped)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-standalone-buildcopy: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-standalone-buildcopy: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/$pkg/data" "$ws/$pkg/tools"
cp "$fixture/data/x.txt" "$ws/$pkg/data/"
cp "$fixture/tools/gen.py" "$ws/$pkg/tools/"
# The genrule has private visibility; open it for the smoke build target below.
sed 's#//visibility:private#//visibility:public#' "$build" > "$ws/$pkg/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sbc", version = "0.0.0")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //$pkg:custom_command_gens_x_gen_c) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the recovered standalone genrule failed (source-tree reads not staged?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# -L: bazel-out / bazel-ws are symlinks into the output base; find won't traverse
# a symlinked path without it (the #754 gate bug; sibling gates use find -L too).
gen_c=$(find -L "$ws" -name x.gen.c -path '*bazel-out*' 2>/dev/null | head -1)
if [ -z "$gen_c" ] || ! grep -qF 'return 7' "$gen_c"; then
    echo "FAIL: recovered gen.c content wrong (expected 'return 7')"
    [ -n "$gen_c" ] && sed 's/^/   /' "$gen_c"
    exit 1
fi

echo "ok  meta-cmake-standalone-buildcopy: the recovered standalone genrule builds + produces gen.c from the source-tree-anchored reads"
