#!/bin/sh
# meta-cmake-probe-genex-compile-language.sh — render+build gate for the
# structural genex probe's language-conditional-property skip.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/probe-genex-compile-language: a
# MULTI-LANGUAGE (C + CXX) project whose INTERFACE library carries
# `$<$<COMPILE_LANGUAGE:CXX>:…>` usage requirements — the textbook
# header-only idiom.
#
# Pre-fix, the structural probe's `file(GENERATE CONTENT
# "$<TARGET_PROPERTY:…,INTERFACE_COMPILE_DEFINITIONS>")` was evaluated
# once per enabled language; the gated arm made the per-language results
# diverge and cmake fatal-errored with "Evaluation file to be written
# multiple times with different content", aborting the whole generate
# step — and the whole conversion, --probe-genex being default-on. The
# fix scans each property's RAW direct value and skips the probe for the
# language-conditional property only; the trace-derived aggregate stands
# for it, and consumers carry the resolved flags via their codemodel
# compile groups regardless.
#
# Asserts the conversion succeeds with the consumer's per-TU truth
# intact (CXXONLY define + -fno-exceptions reach the C++ consumer; the
# interface lib keeps its unconditional define), then bazel-builds and
# RUNS the binary — exit 0 proves both defines reached the compile.
#
# Gating: skips cleanly when cmake isn't on PATH. The divergence abort
# reproduces on any cmake with the probe floor (3.24+); the skip is
# correct everywhere, so the gate runs on any cmake.

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

fixture="$repo_root/converter/testdata/sample-projects/probe-genex-compile-language"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    echo "   a \$<COMPILE_LANGUAGE:…> interface property must not abort the probe"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

# The per-language divergence error is what aborted cmake; assert the
# message never reached stderr so the gate stays meaningful on cmakes
# that might tolerate it.
grep -qF 'Evaluation file to be written multiple times' "$work_dir/convert.stderr" \
    && fail "the probe still emitted a language-divergent file(GENERATE)"

# The consumer's compile groups carry the resolved per-TU truth.
grep -qF '"CXXONLY=1",' "$build" || fail "language-gated interface define lost from the C++ consumer"
grep -qF '"PLAIN=1",' "$build" || fail "unconditional interface define lost from the consumer"
grep -qF '"-fno-exceptions",' "$build" || fail "language-gated interface copt lost from the C++ consumer"
# The synthesized interface lib keeps its trace-derived unconditional
# define (the skipped probe must not blank it).
grep -qF 'defines = ["PLAIN=1"]' "$build" || fail "interface library's unconditional define missing"

# TRANSITIVE reach: a consumer linking a gated dep (its own interface
# clean) must also convert — the probe's link-closure walk skips the
# transitively-gated property instead of aborting the generate step.
trans_fixture="$repo_root/converter/testdata/sample-projects/probe-genex-transitive-language"
trans_build="$work_dir/trans.BUILD"
"$bin_dir/convert-element-cmake" \
    --source-root "$trans_fixture" \
    --out-build "$trans_build" \
    >"$work_dir/trans.stdout" 2>"$work_dir/trans.stderr" || {
    echo "FAIL: convert of the transitive-language fixture exited non-zero (the closure walk should skip the gate)"
    sed 's/^/   stderr: /' "$work_dir/trans.stderr"
    exit 1
}
grep -qF 'Evaluation file to be written multiple times' "$work_dir/trans.stderr" \
    && { echo "FAIL: a TRANSITIVE language gate still diverged the probe"; exit 1; }
grep -qF 'name = "consumer"' "$trans_build" || { echo "FAIL: transitive consumer lib not emitted"; exit 1; }
echo "ok  meta-cmake-probe-genex-compile-language: a transitively-gated consumer converts (link-closure skip)"

echo "ok  meta-cmake-probe-genex-compile-language: language-conditional interface props skip the probe — multi-language project converts"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-probe-genex-compile-language: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-probe-genex-compile-language: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.cc "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "probegenexcompilelanguage", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the converted compile-language project failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# The binary's exit code IS the content check: 0 only when PLAIN and
# CXXONLY both reached the C++ compile.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — a language-gated interface define went missing"
    exit 1
fi

echo "ok  meta-cmake-probe-genex-compile-language: converted project builds and runs clean (per-TU flags intact)"
