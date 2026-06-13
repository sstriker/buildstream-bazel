#!/bin/sh
# meta-cmake-custom-binary-dir.sh — render+build gate for relative-output
# anchoring under add_subdirectory(<src> <custom-binary-dir>).
#
# Directory scopes' build mirrors are NOT always the source-relative path:
# add_subdirectory(sub custom/subbuild) gives sub's scope a
# CMAKE_CURRENT_BINARY_DIR of <build>/custom/subbuild — the exact shape
# FetchContent_MakeAvailable uses for its <name>-src/<name>-build pair. A
# relative configure_file output and a relative file(GENERATE OUTPUT) both
# resolve against that BUILD mirror, so the recoveries must anchor where
# cmake actually writes (the codemodel's per-directory Source/Build pair),
# not at the source-relative path. Pre-fix, the configure_file output
# anchored at sub/ (a path cmake never wrote → silently dropped) and the
# relative file(GENERATE) was dropped outright.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/custom-binary-dir. Asserts (rendered
# BUILD): both generated headers anchor under custom/subbuild/. Bazel-build
# half (bazel >= 7): the consumer #includes both headers and compiles only
# if both genrules exist at the anchored paths and the include wiring
# matches.

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

fixture="$repo_root/converter/testdata/sample-projects/custom-binary-dir"
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

grep -qF 'out = "custom/subbuild/cfg.h"' "$build" \
    || fail "configure_file output not anchored at the custom binary dir"
grep -qF 'out = "custom/subbuild/gen.h"' "$build" \
    || fail "file(GENERATE) relative output not anchored at the custom binary dir"

echo "ok  meta-cmake-custom-binary-dir: relative configure_file + file(GENERATE) outputs anchored at the add_subdirectory custom binary dir"

# The custom binary dir (custom/subbuild) is WIRED as an include (the -I
# that resolves the generated headers) and RECOVERED (its configure_file
# / file(GENERATE) outputs baked above) — it must NOT also draw an
# unsupported-source-path rejection just because the hostSrc header-walk
# can't find a build-tree path. Re-convert in diagnostic mode and assert
# the build-dir include is absent from the rejection report.
rej_dir="$(mktemp -d)"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$rej_dir/BUILD.bazel" \
    --ignore-rejections-for-diagnostics \
    --rejections-report "$rej_dir/rejections.json" \
    >"$rej_dir/convert.stdout" 2>"$rej_dir/convert.stderr" || {
    echo "FAIL: diagnostic-mode convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$rej_dir/convert.stderr"
    rm -rf "$rej_dir"; exit 1
}
if grep -q '"unsupported-source-path"' "$rej_dir/rejections.json" \
        && grep -q 'custom/subbuild' "$rej_dir/rejections.json"; then
    echo "FAIL: wired+recovered build-dir include custom/subbuild drew an unsupported-source-path rejection"
    sed 's/^/   /' "$rej_dir/rejections.json"
    rm -rf "$rej_dir"; exit 1
fi
rm -rf "$rej_dir"

echo "ok  meta-cmake-custom-binary-dir: the wired+recovered build-dir include draws no unsupported-source-path rejection"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-custom-binary-dir: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-custom-binary-dir: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp -r "$fixture/." "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "custombindir", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //...) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the consumer of the custom-binary-dir generated headers failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-custom-binary-dir: the consumer compiles against both anchored generated headers (no cmake)"
