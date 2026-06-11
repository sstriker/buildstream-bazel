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
# synthesizing a MIRROR of cmake's generated cmake_pch.h[xx] — a write_file
# header carrying `#pragma GCC system_header` plus the declared #includes
# in order — force-included at the cmake_pch pair's original argv position;
# the speed half stays operator-side (docs/operator-toolchain-features.md),
# kept greppable via the cmake-codegen-pch tag.
#
# Drives convert-element-cmake against converter/testdata/sample-projects/pch:
# a declaring library (core, PCH = pch.h + <vector>, compiled -Werror with
# a deliberate warning-trigger INSIDE pch.h — the suppression probe), a
# REUSE_FROM consumer (user) whose codemodel carries NO precompileHeaders —
# its PCH arrives only as the `-include <owner>.dir/cmake_pch.hxx`
# fragment, the shape that used to be dropped silently with no tag — and a
# mixed target with a SKIP_PRECOMPILE_HEADERS TU (compiles WITHOUT the PCH
# via the same-language compile-group split).
#
# Asserts (rendered BUILD): the mirror rule (pragma + ordered includes),
# the mirror force-include on declaring + REUSE_FROM targets, the SKIP
# TU's sub-library staying include-free, the tags, and no cmake_pch
# BUILD-DIR artifact leaking. Bazel-build half (bazel >= 7) builds //... —
# the PCH-reliant TUs are include-free (compile ONLY through the forced
# include), the -Werror probe fails the build if the mirror loses cmake's
# warning suppression, and the SKIP probe #errors if the PCH leaks in.

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

# 1. The declaring target force-includes the synthesized MIRROR of
# cmake's cmake_pch.hxx (single -include at the original argv position).
grep -qF '"-include"' "$build" || fail "no -include copts emitted (forced-include lift missing)"
grep -qF '"cmake_pch/core/cmake_pch.hxx",' "$build" || fail "mirror header missing from the forced include"
# 2. The mirror rule reproduces cmake's header: system_header pragma
# (warning suppression) plus the declared #includes in order.
grep -qF '#pragma GCC system_header' "$build" || fail "mirror missing the system_header pragma (warning suppression lost)"
grep -qF '#include \"pch.h\"' "$build" || fail "pch.h missing from the mirror body"
grep -qF '#include <vector>' "$build" || fail "angle-form <vector> entry missing from the mirror body"
# 3. No cmake_pch BUILD-DIR artifact leaks into the rendered output
# (the mirror's own cmake_pch/ tree is ours; the CMakeFiles form is the
# convert-time leak).
grep -q 'CMakeFiles.*cmake_pch' "$build" \
    && fail "cmake_pch build-dir artifact leaked into the BUILD"
# 4. Declaring target, REUSE_FROM consumer, and the SKIP-probe target are
# all tagged.
tag_count=$(grep -c "cmake-codegen-pch" "$build" || true)
[ "$tag_count" -ge 5 ] || fail "expected the cmake-codegen-pch tag on core, user, mixed, unit_test AND tool (got $tag_count)"
# 5. The REUSE_FROM consumer (user) got the OWNER's mirror — same path,
# shared rule.
awk '/name = "user"/{f=1} f{print} f&&/^\)/{exit}' "$build" | grep -qF '"cmake_pch/core/cmake_pch.hxx",' \
    || fail "REUSE_FROM consumer (user) missing the owner's mirror include"
# 6. SKIP_PRECOMPILE_HEADERS: skip.cpp's sub-library compiles WITHOUT the
# forced include (the same-language compile-group split routes it apart).
awk '/name = "mixed_cxx_1"/{f=1} f{print} f&&/^\)/{exit}' "$build" | grep -qF '"-include"' \
    && fail "SKIP_PRECOMPILE_HEADERS TU still carries the forced include"
# 7. TEST-BINARY shape: the cc_test carries its mirror include, and the
# PCH-CREATOR compile group's `-x c++-header` fragments must NOT leak
# anywhere (they'd compile every TU as a header).
awk '/name = "unit_test"/{f=1} f{print} f&&/^\)/{exit}' "$build" | grep -qF '"cmake_pch/unit_test/cmake_pch.hxx",' \
    || fail "PCH-declaring cc_test missing its mirror include"
grep -qF 'c++-header' "$build" \
    && fail "PCH-creator '-x c++-header' fragments leaked into the BUILD"
# 8. Cross-kind REUSE_FROM: the owner edge routes to data (an executable
# in deps is illegal in Bazel) while the consumer still force-includes
# the owner's mirror.
awk '/name = "tool"/{f=1} f{print} f&&/^\)/{exit}' "$build" | grep -qF '":unit_test"' \
    && fail "REUSE_FROM owner target referenced by the consumer (deps is illegal for executable kinds; data poisons via testonly) — the mirror FILE in srcs is the only edge needed"
awk '/name = "tool"/{f=1} f{print} f&&/^\)/{exit}' "$build" | grep -qF '"cmake_pch/unit_test/cmake_pch.hxx",' \
    || fail "cross-kind REUSE_FROM consumer missing the owner's mirror"

echo "ok  meta-cmake-pch: target_precompile_headers lifted to the mirror forced include (declaring + REUSE_FROM + SKIP + test-binary shapes)"

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
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //...) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the PCH-reliant TUs failed — forced include regressed, OR the -Werror suppression probe fired (mirror system_header not honored), OR the SKIP probe leaked"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-pch: PCH-reliant sources compile under Bazel via the mirror (incl. -Werror suppression + SKIP probes; no cmake)"
