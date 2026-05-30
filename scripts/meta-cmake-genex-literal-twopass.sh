#!/bin/sh
# meta-cmake-genex-literal-twopass.sh — render gate for the
# generalized-genex warm second configure pass (ROADMAP.md
# "generalized genex literal probe / two-pass").
#
# Proves that an ARBITRARY genex literal outside the structural
# probe's fixed set — here $<TARGET_PROPERTY:app,APP_GENDIR> in a
# file(GENERATE) OUTPUT path — which the Go-side (a) evaluator and
# the per-target structural probe both REFUSE, is resolved
# end-to-end via the warm second configure pass.
#
# The structural probe (probe-genex.cmake) only captures a fixed
# set of shapes (TARGET_FILE / TARGET_OBJECTS / INTERFACE_*); an
# arbitrary custom-property read isn't among them, and the v1
# Go-side evaluator doesn't model arbitrary TARGET_PROPERTY. So
# pass 1 can't resolve the OUTPUT path and DROPS the file(GENERATE)
# call. With --two-pass-genex (default ON), convert-element-cmake
# records the unresolved literal, runs a second cmake configure
# against the SAME (warm) build dir injecting a file(GENERATE)
# literal-probe hook, reads cmake's own resolved value back
# ("gen_out"), and re-lifts — so the call now lands as a genrule
# whose outs is the resolved path "gen_out/manifest.txt".
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/genex-literal-twopass (a
# STATIC_LIBRARY `app` with a custom APP_GENDIR property and a
# file(GENERATE) whose OUTPUT embeds $<TARGET_PROPERTY:app,APP_GENDIR>).
#
# Asserts:
#   1. With --two-pass-genex (default), convert exits 0, the stderr
#      announces the warm second pass, and the emitted BUILD carries
#      a genrule with outs = ["gen_out/manifest.txt"] — the OUTPUT
#      genex resolved to the static path.
#   2. Negative / load-bearing: with --two-pass-genex=false the same
#      file(GENERATE) is DROPPED (no gen_out/manifest.txt genrule),
#      pinning the second pass as the thing that resolves it — so
#      assertion 1 can't pass vacuously.
#
# cmake 3.24+ is required for the CMAKE_PROJECT_TOP_LEVEL_INCLUDES
# hook the probe rides; the gate skips cleanly when cmake isn't on
# PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/genex-literal-twopass"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

# --- Assertion 1: two-pass ON resolves the OUTPUT genex. ---
out_on="$work_dir/BUILD.on"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_on" \
    --lift-configure-file=true \
    --two-pass-genex=true \
    >"$work_dir/on.stdout" 2>"$work_dir/on.stderr" || {
    echo "FAIL: convert-element-cmake (--two-pass-genex) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/on.stderr"
    exit 1
}

if ! grep -q 'warm second configure pass' "$work_dir/on.stderr"; then
    echo "FAIL: expected the warm second configure pass to announce itself on stderr"
    echo "   (pass 1 should have recorded the unresolved \$<TARGET_PROPERTY:app,APP_GENDIR>)"
    sed 's/^/   stderr: /' "$work_dir/on.stderr"
    exit 1
fi

if ! grep -qF 'outs = ["gen_out/manifest.txt"]' "$out_on"; then
    echo "FAIL: emitted BUILD missing the resolved genrule outs = [\"gen_out/manifest.txt\"]"
    echo "   the warm second pass should have resolved the OUTPUT genex to gen_out/"
    echo "--- generated BUILD ---"
    sed 's/^/   /' "$out_on"
    exit 1
fi

# --- Assertion 2 (load-bearing): two-pass OFF drops the call. ---
out_off="$work_dir/BUILD.off"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_off" \
    --lift-configure-file=true \
    --two-pass-genex=false \
    >"$work_dir/off.stdout" 2>"$work_dir/off.stderr" || {
    echo "FAIL: convert-element-cmake (--two-pass-genex=false) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/off.stderr"
    exit 1
}

if grep -qF 'gen_out/manifest.txt' "$out_off"; then
    echo "FAIL: --two-pass-genex=false still emitted gen_out/manifest.txt"
    echo "   without the second pass the unresolved-OUTPUT file(GENERATE) must be"
    echo "   dropped — assertion 1 would otherwise be passing vacuously"
    echo "--- generated BUILD (two-pass off) ---"
    sed 's/^/   /' "$out_off"
    exit 1
fi

echo "ok  meta-cmake-genex-literal-twopass: \$<TARGET_PROPERTY:app,APP_GENDIR> in a file(GENERATE) OUTPUT resolved via the warm second configure pass (outs = gen_out/manifest.txt); load-bearing under --two-pass-genex=false"
