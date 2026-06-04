#!/bin/sh
# meta-cmake-export-header.sh — render+build gate for the
# generate_export_header recovery + split-packages include wiring.
#
# CMake's GenerateExportHeader module (the near-universal symbol-visibility
# idiom: nearly every cmake C++ library uses it) calls configure_file() from
# CMake's own module dir against the fixed template exportheader.cmake.in,
# producing <name>_export.h. Two things must happen for a consumer to build:
#   1. Recovery: the converter must NOT drop that configure_file just because
#      its call-site is outside the project tree (classifyConfigureFile keys on
#      the exportheader.cmake.in template basename).
#   2. Include wiring: the generated header is #included by BARE name, so its
#      directory (cmake's CMAKE_CURRENT_BINARY_DIR) must land on the include
#      path — even under --split-packages, where the header sits in a subdir
#      that must become its own include-root header lib.
#
# Drives convert-element-cmake --split-packages against
# converter/testdata/sample-projects/export-header (a lib whose source #includes
# the bare generate_export_header output, generated into a SUBDIR to stress the
# split include-root assignment).
#
# Asserts (rendered BUILD): the export-header genrule is emitted, no refusal,
# and the subdir package owns it with includes=["."]. Bazel-build half
# (bazel >= 7) builds //lib:mylib, proving the bare include resolves — the
# regression this gate guards (without the include wiring the header lands in
# hdrs but the compile can't find it).

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

fixture="$repo_root/converter/testdata/sample-projects/export-header"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --split-packages \
    --out-build "$work_dir/BUILD.bazel" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake --split-packages exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- lib/sub/BUILD.bazel ---"
    sed 's/^/   /' "$work_dir/lib/sub/BUILD.bazel" 2>/dev/null || true
    exit 1
}

sub_build="$work_dir/lib/sub/BUILD.bazel"
[ -f "$sub_build" ] || fail "no lib/sub package emitted (export header's include-root subpackage missing)"

# 1. The export header is produced in the subdir package as mylib_export.h.
grep -qF 'out = "mylib_export.h"' "$sub_build" \
    || fail "export header producer (out = mylib_export.h) missing from lib/sub"
# 2. The subdir is an include-root header lib carrying the export header.
grep -qF '"mylib_export.h"' "$sub_build" || fail "export header not wired into lib/sub header lib"
grep -qF 'includes = ["."]' "$sub_build" || fail "lib/sub header lib missing includes=[\".\"]"
# 3. No refusal of the generate_export_header configure_file. Match the typed
# refusal-code prefix ("unsupported-…") rather than the bare word, so an
# unrelated message that happens to contain "unsupported" can't trip the gate.
grep -q "unsupported-" "$work_dir/convert.stderr" \
    && fail "converter emitted a typed refusal (export header should recover cleanly)"

echo "ok  meta-cmake-export-header: generate_export_header recovered + wired into a split-packages include-root"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-export-header: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-export-header: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp -r "$fixture/." "$ws/"
cp "$work_dir/lib/BUILD.bazel" "$ws/lib/BUILD.bazel"
cp "$sub_build" "$ws/lib/sub/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "exporthdr", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //lib:mylib) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the library whose source #includes the bare generate_export_header output failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-export-header: the converted library compiles with the bare export-header include resolved (no cmake)"
