#!/bin/sh
# meta-cmake-nested-cmake.sh — render+build gate for the nested-cmake lift
# (the superbuild-at-configure idiom).
#
# The fixture's outer configure runs `execute_process(${CMAKE_COMMAND} -S
# sub -B <build>/subbuild)` + `cmake --build`, then the outer app consumes
# the sub-build's artifacts: links subbuild/libsublib.a and includes the
# nested configure-generated sub_config.h. Historically both calls
# Tier-1-refused, making the whole element unconvertible.
#
# The lift: pass 1 detects the nested (src, build) pair from the trace; the
# warm second pass stages File API queries into the nested build dir and
# re-configures (the nested cmake re-runs and writes a codemodel reply);
# the nested reply lowers recursively with labels anchored at the OUTER
# root and merges — the nested cc_library lands in the outer BUILD, the
# archive link fragment wires to its label, and the nested
# configure-generated header bakes for the outer include consumer.
#
# Asserts the merged/baked/wired shapes render, then bazel-builds and RUNS
# the consumer binary — exit 0 proves the nested lib linked and the baked
# header carries the right value.

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

fixture="$repo_root/converter/testdata/sample-projects/nested-cmake"
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

grep -qF 'name = "sublib"' "$build" || fail "nested target not merged into the outer package"
grep -qF '"sub/sub.c"' "$build" || fail "nested target's srcs not re-anchored at the outer root"
grep -qF '"cmake-codegen-nested-cmake"' "$build" || fail "nested-cmake audit tag missing on the merged target"
grep -qF 'deps = [":sublib"]' "$build" || fail "nested archive link fragment not wired to the merged target's label"
grep -qF 'out = "subbuild/sub_config.h"' "$build" || fail "nested configure-generated header not recovered"
# The driver's traced re-configure of the nested dir hands the nested
# lowering a real trace, so the header recovers via the NATIVE
# configure_file channel (not the trace-less build-dir-bake fallback,
# and not the cmake-codegen-nested-cmake-bake last resort for headers
# only OUTER targets consume), re-homed under subbuild/ by the merge.
grep -qF '"cmake-codegen-configure-file"' "$build" || fail "configure_file recovery facet missing — the nested trace didn't reach the nested lowering"
grep -qF '"subbuild/sub_config.h",' "$build" || fail "recovered header not attributed to the outer consumer"
grep -FA3 'hdrs = [' "$build" | grep -qF '"subbuild/sub_config.h",' || fail "recovered header not attached to the nested consumer's hdrs (sub.c's include would be undeclared)"
# The nested execute_process codegen (cmake -E copy) is liftable ONLY
# from the nested trace; assert the cp genrule landed with the
# label-root-anchored template src and wired into the nested lib.
grep -qF '"cmake-codegen-execute-process-op=copy"' "$build" || fail "nested execute_process codegen not lifted (trace didn't drive the exec recovery)"
grep -qF '$(location sub/sub_extra.c.in)' "$build" || fail "nested cp genrule's template not re-anchored at the label root"
grep -qF '"subbuild/sub_extra.c",' "$build" || fail "lifted codegen output not wired into the nested consumer's srcs"
grep -q 'unsupported-execute-process' "$work_dir/convert.stderr" \
    && fail "nested cmake calls still refuse (the lift should cover both)"

# --- Superbuild CHAIN (depth 2): sub's configure runs its OWN nested
# cmake (subsub). The driver worklist detects it from sub's trace,
# stages + traced-re-configures the grandchild dir directly, and the
# recursive lowering composes the re-homes: the grandchild's
# configure_file recovery lands doubly re-homed under
# subbuild/subsubbuild/ with a chain-composed rule name, and sub.c's
# include of it re-points through both levels.
grep -qF 'out = "subbuild/subsubbuild/subsub_config.h"' "$build" || fail "grandchild configure-generated header not recovered/re-homed through the chain"
grep -qF '"subbuild/subsubbuild/subsub_config.h",' "$build" || fail "grandchild header not attributed to its consumer"
grep -qF '"subbuild/subsubbuild",' "$build" || fail "nested consumer's grandchild-build include dir not re-homed through the chain"
grep -q 'detected but not lifted.*subsubbuild' "$work_dir/convert.stderr" \
    && fail "grandchild nested build still warns not-lifted (the worklist should lift it)"

echo "ok  meta-cmake-nested-cmake: nested configure+build lifted — targets merged, archive wired, generated header baked (chain depth 2 incl.)"

# --- configure_file LIFT tier inside nested lowerings ---
# A second convert under --lift-configure-file (the operator opt-in,
# threaded into the nested options): the nested and GRANDCHILD
# configure_file recoveries emit the dynamic values-dict
# cmake_configure_file rule instead of the convert-time byte bake, with
# the template umbrella-re-anchored at the outer label root and the out
# re-homed through the chain. Render-only assertions — the build half
# below stays on the default (bake) pass; lift-tier builds (which need
# //tools:cmake-configure-file staged) are exercised by the write-a
# gates.
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --lift-configure-file \
    --out-build "$work_dir/lift/BUILD.bazel" \
    >"$work_dir/lift-convert.stdout" 2>"$work_dir/lift-convert.stderr" || {
    echo "FAIL: convert-element-cmake --lift-configure-file exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/lift-convert.stderr"
    exit 1
}
lift_build="$work_dir/lift/BUILD.bazel"
lfail() {
    echo "FAIL: $1"
    echo "   --- lift BUILD.bazel ---"
    sed 's/^/   /' "$lift_build" 2>/dev/null || true
    exit 1
}
grep -qF 'cmake_configure_file(' "$lift_build" || lfail "lift tier didn't fire inside the nested lowering"
grep -qF 'out = "subbuild/sub_config.h"' "$lift_build" || lfail "nested lift-tier out not re-homed under subbuild/"
grep -qF 'template = "sub/sub_config.h.in"' "$lift_build" || lfail "nested lift-tier template not umbrella-re-anchored at the outer label root"
grep -qF 'values = {"SUB_VALUE": "7"}' "$lift_build" || lfail "nested lift-tier values dict missing — the dynamic substitution payoff"
grep -qF 'out = "subbuild/subsubbuild/subsub_config.h"' "$lift_build" || lfail "grandchild lift-tier out not re-homed through the chain"
grep -qF 'template = "sub/subsub/subsub_config.h.in"' "$lift_build" || lfail "grandchild lift-tier template not umbrella-re-anchored"
grep -qF 'values = {"SUBSUB_VALUE": "11"}' "$lift_build" || lfail "grandchild lift-tier values dict missing"

echo "ok  meta-cmake-nested-cmake: configure_file LIFT tier fires inside nested lowerings (chain depth 2 incl.)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-nested-cmake: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-nested-cmake: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/sub"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/sub/sub.c "$fixture"/sub/sub_extra.c.in "$ws/sub/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "nestedcmake", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the converted nested-cmake project failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# The binary's exit code IS the content check: it returns 0 only when the
# nested lib's symbol links AND the baked headers carry SUB_VALUE=7 plus
# the GRANDCHILD's SUBSUB_VALUE=11 (sub_value() returns 7 + 11).
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — nested lib or baked header content wrong"
    exit 1
fi

echo "ok  meta-cmake-nested-cmake: merged nested lib links and the consumer runs clean (no cmake at build time)"
