#!/bin/sh
# meta-cmake-pch.sh — render+build gate for the target_precompile_headers
# forced-include lift.
#
# cmake's PCH machinery is two effects welded together: a FORCED INCLUDE
# (every TU compiles as if the generated cmake_pch.h[xx] — which #includes
# the declared headers in order — were its first line) and a compile-speed
# optimization (the .gch/.pch precompilation). The forced include is a
# correctness input: sources legitimately rely on the PCH for declarations
# and macros they never #include themselves. The converter preserves it by
# expanding the declared header list into ordered `-include` copts pairs;
# the speed half stays operator-side (docs/operator-toolchain-features.md),
# kept greppable via the cmake-codegen-pch tag.
#
# Drives convert-element-cmake against converter/testdata/sample-projects/pch:
# a declaring library (core, PCH = pch.h + <vector>) and a REUSE_FROM
# consumer (user) whose codemodel carries NO precompileHeaders — its PCH
# arrives only as the `-include <owner>.dir/cmake_pch.hxx` fragment, the
# shape that used to be dropped silently with no tag.
#
# Asserts (rendered BUILD): both targets carry the forced-include expansion
# and the cmake-codegen-pch tag, and no cmake_pch build-dir artifact leaks
# into the output. Bazel-build half (bazel >= 7) builds //... — both
# fixture TUs are deliberately include-free and compile ONLY through the
# forced include, so a regression to the old drop behavior fails the build.

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

fixture="$repo_root/converter/testdata/sample-projects/pch"
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

# 1. The declaring target's PCH list is expanded into -include pairs.
grep -qF '"-include"' "$build" || fail "no -include copts emitted (forced-include lift missing)"
grep -qF '"pch.h"' "$build" || fail "pch.h missing from the forced-include expansion"
grep -qF '"vector"' "$build" || fail "angle-form <vector> entry missing from the expansion"
# 2. No cmake_pch build-dir artifact leaks into the rendered output.
# Comment lines are excluded: the fixture's own CMakeLists comments mention
# cmake_pch and ride into the BUILD via --emit-source-comments.
grep -v '^[[:space:]]*#' "$build" | grep -q "cmake_pch" \
    && fail "cmake_pch build-dir artifact leaked into the BUILD"
# 3. Both the declaring target and the REUSE_FROM consumer are tagged.
tag_count=$(grep -c "cmake-codegen-pch" "$build" || true)
[ "$tag_count" -ge 2 ] || fail "expected the cmake-codegen-pch tag on core AND the REUSE_FROM consumer (got $tag_count)"
# 4. The REUSE_FROM consumer (user) got the owner's expansion: its rule
# block must carry -include. Extract user's block (from its name line to
# the closing paren at column 0).
awk '/name = "user"/{f=1} f{print} f&&/^\)/{exit}' "$build" | grep -qF '"-include"' \
    || fail "REUSE_FROM consumer (user) missing the owner's forced-include expansion"

echo "ok  meta-cmake-pch: target_precompile_headers lifted to forced-include copts (declaring + REUSE_FROM)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-pch: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-pch: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/*.cpp "$fixture"/*.h "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "pchsample", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //...) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the PCH-reliant TUs failed — the forced-include lift regressed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-pch: include-free PCH-reliant sources compile under Bazel via the lifted -include copts (no cmake)"
