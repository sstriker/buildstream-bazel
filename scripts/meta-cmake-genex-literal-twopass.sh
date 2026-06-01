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

# --- Assertion 1b (tier b′): two-pass ON resolves the CONTENT-body
# adjacent genexes the structured-capture extractor can't anchor. The
# content_adjacent.txt genrule must lift (cmake-codegen-genex-resolved)
# rather than bake the rendered "alphabeta\n" bytes. ---
if ! grep -qF 'cmake-codegen-genex-resolved' "$out_on"; then
    echo "FAIL: two-pass ON did not resolve the adjacent CONTENT-body genexes"
    echo "   expected gen_content_adjacent_txt to carry cmake-codegen-genex-resolved"
    echo "--- generated BUILD ---"
    sed 's/^/   /' "$out_on"
    exit 1
fi
# YWxwaGFiZXRhCg== is base64("alphabeta\n") — the rendered bytes the
# legacy bake would embed. Their ABSENCE proves the body lifted to a
# literal-replace map (template + per-genex values) instead of baking.
if grep -qF 'YWxwaGFiZXRhCg==' "$out_on"; then
    echo "FAIL: two-pass ON baked the rendered CONTENT bytes instead of lifting"
    echo "   the per-literal probe should have produced a --genex-values map,"
    echo "   keeping the rendered \"alphabeta\" bytes out of srckey"
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

# --- Assertion 2b (load-bearing for tier b′): with two-pass OFF the
# content_adjacent.txt body falls to the legacy bake — the genex tag
# flips to cmake-codegen-genex-unresolved and the rendered bytes are
# embedded. This proves the second pass is what flips it to resolved
# in assertion 1b (not some other always-on path). The OUTPUT here is
# static, so the genrule is still emitted (only the body shape flips),
# unlike the manifest.txt OUTPUT case which drops entirely. ---
if ! grep -qF 'cmake-codegen-genex-unresolved' "$out_off"; then
    echo "FAIL: --two-pass-genex=false should bake the adjacent CONTENT genexes"
    echo "   (cmake-codegen-genex-unresolved) — assertion 1b would otherwise pass vacuously"
    echo "--- generated BUILD (two-pass off) ---"
    sed 's/^/   /' "$out_off"
    exit 1
fi
# The bake lowers to skylib write_file with a HUMAN-READABLE content
# line list (the base64 -> write_file maintainability change), not the
# legacy `echo <base64> | base64 -d`. Assert the readable shape: a
# write_file carrying the literal "alphabeta" line and NO base64 blob.
if ! grep -qF 'write_file(' "$out_off"; then
    echo "FAIL: --two-pass-genex=false should bake content_adjacent via skylib write_file"
    echo "--- generated BUILD (two-pass off) ---"
    sed 's/^/   /' "$out_off"
    exit 1
fi
if ! grep -qF '"alphabeta"' "$out_off"; then
    echo "FAIL: the write_file bake should carry the readable 'alphabeta' content line"
    echo "--- generated BUILD (two-pass off) ---"
    sed 's/^/   /' "$out_off"
    exit 1
fi
if grep -qF 'YWxwaGFiZXRhCg==' "$out_off"; then
    echo "FAIL: the bake should be readable write_file content, not a base64 blob"
    echo "--- generated BUILD (two-pass off) ---"
    sed 's/^/   /' "$out_off"
    exit 1
fi

echo "ok  meta-cmake-genex-literal-twopass: \$<TARGET_PROPERTY:app,APP_GENDIR> in a file(GENERATE) OUTPUT resolved via the warm second configure pass (outs = gen_out/manifest.txt); CONTENT-body adjacent genexes resolved via per-literal probe (tier b′, cmake-codegen-genex-resolved); the bake path lowers to readable skylib write_file; all load-bearing under --two-pass-genex=false"
