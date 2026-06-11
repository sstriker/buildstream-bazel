#!/bin/sh
# meta-cmake-nested-cmake-workdir.sh — render+build gate for the nested-cmake
# lift via the WORKING_DIRECTORY + positional-source idiom (the dominant
# "download/build at configure" shape: cryptoauthlib's mbedtls downloader,
# the googletest DownloadProject trick).
#
# Unlike the sibling meta-cmake-nested-cmake.sh fixture (which spells the
# nested build with absolute -S/-B), here the outer configure runs
# `execute_process(${CMAKE_COMMAND} -G Ninja <src>` with the source as a
# POSITIONAL operand (no -S) and the build dir supplied by WORKING_DIRECTORY
# (no -B), then `cmake --build .` — the relative "." anchored against the
# same WORKING_DIRECTORY. Historically the configure refused as an opaque
# execute_process (the recognizer needed -S/-B) and the `--build .` refused
# on the relative-dir + WORKING_DIRECTORY combination; now both resolve
# against WORKING_DIRECTORY and lift through the same warm-pass machinery.
#
# Asserts the merged/baked/wired shapes render, then bazel-builds and RUNS
# the consumer binary — exit 0 proves the nested lib linked through the
# WORKING_DIRECTORY-form recognition and the baked header carries the value.

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

fixture="$repo_root/converter/testdata/sample-projects/nested-cmake-workdir"
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

grep -qF 'name = "sublib"' "$build" || fail "nested target not merged (WORKING_DIRECTORY form not recognized)"
grep -qF '"sub/sub.c"' "$build" || fail "nested target's srcs not re-anchored at the outer root"
grep -qF '"cmake-codegen-nested-cmake"' "$build" || fail "nested-cmake audit tag missing on the merged target"
grep -qF 'deps = [":sublib"]' "$build" || fail "nested archive link fragment not wired to the merged target's label"
grep -qF 'out = "subbuild/sub_config.h"' "$build" || fail "nested configure-generated header not baked"
grep -qF '"subbuild/sub_config.h",' "$build" || fail "baked header not attributed to the outer consumer"
grep -q 'unsupported-execute-process' "$work_dir/convert.stderr" \
    && fail "nested cmake calls still refuse (the WORKING_DIRECTORY form should be recognized)"

echo "ok  meta-cmake-nested-cmake-workdir: WORKING_DIRECTORY + positional-source nested build lifted — targets merged, archive wired, header baked"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-nested-cmake-workdir: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-nested-cmake-workdir: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/sub"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/sub/sub.c "$ws/sub/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "nestedcmakewd", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the converted nested-cmake-workdir project failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# The binary's exit code IS the content check: 0 only when the nested lib's
# symbol links AND the baked header carries SUB_VALUE=7.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — nested lib or baked header content wrong"
    exit 1
fi

echo "ok  meta-cmake-nested-cmake-workdir: merged nested lib links and the consumer runs clean (no cmake at build time)"
