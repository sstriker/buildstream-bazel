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
grep -qF 'hdrs = ["subbuild/sub_config.h"]' "$build" || fail "recovered header not attached to the nested consumer's hdrs (sub.c's include would be undeclared)"
# The nested execute_process codegen (cmake -E copy) is liftable ONLY
# from the nested trace; assert the cp genrule landed with the
# label-root-anchored template src and wired into the nested lib.
grep -qF '"cmake-codegen-execute-process-op=copy"' "$build" || fail "nested execute_process codegen not lifted (trace didn't drive the exec recovery)"
grep -qF '$(location sub/sub_extra.c.in)' "$build" || fail "nested cp genrule's template not re-anchored at the label root"
grep -qF '"subbuild/sub_extra.c",' "$build" || fail "lifted codegen output not wired into the nested consumer's srcs"
grep -q 'unsupported-execute-process' "$work_dir/convert.stderr" \
    && fail "nested cmake calls still refuse (the lift should cover both)"

echo "ok  meta-cmake-nested-cmake: nested configure+build lifted — targets merged, archive wired, generated header baked"

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
# nested lib's symbol links AND the baked header carries SUB_VALUE=7.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — nested lib or baked header content wrong"
    exit 1
fi

echo "ok  meta-cmake-nested-cmake: merged nested lib links and the consumer runs clean (no cmake at build time)"
