#!/bin/sh
# meta-cmake-execute-process-rescue.sh — render gate for the
# Phase 4 execute_process dump-vars rescue.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/execute-process-probe-rescue,
# a CMakeLists with a top-level execute_process PROBE whose stdout
# is captured into OUTPUT_VARIABLE PROBE_RESULT, and a
# configure_file whose @PROBE_RESULT@ marker consumes that value.
#
# The probe call (`python3 -c "print('probe-value-42')"`,
# OUTPUT_VARIABLE, no OUTPUT_FILE) classifies as BucketProbe in
# the converter's Classify(). recoverExecuteProcess SKIPS a
# BucketProbe call that produces no file artifact — a host/toolchain
# probe is never a build input, so emitting nothing is faithful (the
# broadened stance). The probe's captured value still reaches the
# downstream configure_file independently: from cmakeVars when the
# dump-vars hook caught it, else byte-reverse-engineered from the
# (template, rendered) pair by the configure_file lift.
#
# This gate proves the rescue value genuinely re-renders at Bazel
# time (not just that conversion stops refusing): the recovered
# configure_file is the LIFTED cmake_configure_file shape (a
# `template` label on the .h.in, //tools:cmake-configure-file as the
# `tool` invoked at build time, a readable `values` dict). PROBE_RESULT
# rides in that values dict, so re-running the substitution tool over
# the template + values materializes `probe-value-42` into config.h —
# the contract the rescue owes its downstream consumer.
#
# Asserts:
#   1. convert-element-cmake exits 0.
#   2. NO `unsupported-execute-process` appears in stderr (the
#      BucketProbe call skipped instead of refusing).
#   3. The recovered configure_file is the lifted re-rendering
#      shape (//tools:cmake-configure-file + cmake-codegen-lifted),
#      and re-running cmake-configure-file over the template + the
#      rule's captured values dict materializes `probe-value-42`
#      — i.e. the probe value reaches a real Bazel-time re-render.
#   4. The broadened skip holds without the dump-vars capture too:
#      with --dump-vars=false the probe still SKIPS (convert exits 0,
#      no Tier-1 refusal) and `probe-value-42` still materializes —
#      the configure_file recovers PROBE_RESULT by byte-reverse-
#      engineering the (template, rendered) pair. Pins the broaden:
#      an uncaptured host/toolchain probe is benign, not a refusal.
#
# cmake-availability gating: skips cleanly when no cmake >= 3.24
# is on PATH (the architectural floor for the dump-vars hook the
# rescue consumes), and when python3 is absent (the fixture's
# probe driver). Mirrors the skip shape of the sibling gates.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo "skip: python3 not on PATH (the fixture's execute_process probe driver)"
    exit 0
fi

# Build the converter + the Bazel-time substitution tool the
# lifted configure_file genrule invokes. The gate runs the tool
# itself to prove the probe value materializes through the
# re-render (assertion 3).
bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/cmake-configure-file" ./cmd/cmake-configure-file

fixture="$repo_root/converter/testdata/sample-projects/execute-process-probe-rescue"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

# Assertion (1)+(2): convert exits 0 and the rescue suppresses the
# BucketProbe Tier-1 refusal. --lift-configure-file selects the
# re-rendering genrule shape so the captured PROBE_RESULT becomes a
# Bazel-time substitution input rather than baked rendered bytes.
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --lift-configure-file \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    echo "   the Phase 4 dump-vars rescue should have skipped the"
    echo "   BucketProbe refusal for the captured PROBE_RESULT"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

if grep -q 'unsupported-execute-process' "$work_dir/convert.stderr"; then
    echo "FAIL: unsupported-execute-process surfaced despite the rescue"
    echo "   PROBE_RESULT was captured by dump-vars; the BucketProbe"
    echo "   refusal should have been skipped"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
fi

# Assertion (3a): the recovered configure_file is the lifted
# re-rendering shape — a cmake_configure_file rule with the template
# label, //tools:cmake-configure-file as its tool, cmake-codegen-lifted.
for marker in \
    'name = "gen_config_h"' \
    '"src/config.h.in"' \
    '"//tools:cmake-configure-file"' \
    'cmake-codegen-lifted'; do
    if ! grep -qF -- "$marker" "$out_build"; then
        echo "FAIL: lifted configure_file genrule missing marker: $marker"
        echo "   the rescued probe value only re-renders at Bazel time"
        echo "   when the configure_file lifts to the tool-invoking shape"
        echo "   --- generated BUILD ---"
        sed 's/^/   /' "$out_build"
        exit 1
    fi
done

# Assertion (3b): the probe value rides in the cmake_configure_file
# rule's readable `values` dict, and re-running cmake-configure-file
# over the template + those values materializes `probe-value-42`. This
# is the live contract — extract the `values = {...}` Starlark dict from
# the gen_config_h rule (a Starlark string-dict literal parses as a
# Python dict), write it as JSON, and re-render exactly as the Bazel
# action would (the @ONLY template lifts with --at-only).
python3 - "$out_build" "$work_dir/values.json" <<'PYEOF'
import sys, json, ast
txt = open(sys.argv[1]).read()
start = txt.find('name = "gen_config_h"')
if start < 0:
    sys.stderr.write("could not locate the gen_config_h rule\n")
    sys.exit(1)
vi = txt.find('values = {', start)
if vi < 0:
    sys.stderr.write("could not locate the values dict in the gen_config_h rule\n")
    sys.exit(1)
brace = txt.index('{', vi)
# Brace-match to the dict close, but skip braces inside string literals
# (a cmake variable value can legitimately contain { or }), honoring
# backslash escapes, so the count tracks only structural braces.
depth, end, in_str, i = 0, None, None, brace
while i < len(txt):
    c = txt[i]
    if in_str is not None:
        if c == "\\":
            i += 2
            continue
        if c == in_str:
            in_str = None
    elif c == '"' or c == "'":
        in_str = c
    elif c == "{":
        depth += 1
    elif c == "}":
        depth -= 1
        if depth == 0:
            end = i + 1
            break
    i += 1
if end is None:
    sys.stderr.write("unbalanced braces in the gen_config_h values dict\n")
    sys.exit(1)
values = ast.literal_eval(txt[brace:end])
json.dump(values, open(sys.argv[2], "w"))
PYEOF

rendered="$work_dir/config.h.rendered"
"$bin_dir/cmake-configure-file" \
    --at-only \
    --values="$work_dir/values.json" \
    "$fixture/src/config.h.in" \
    "$rendered" \
    >"$work_dir/render.stdout" 2>"$work_dir/render.stderr" || {
    echo "FAIL: cmake-configure-file re-render exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/render.stderr"
    exit 1
}

if ! grep -q 'probe-value-42' "$rendered"; then
    echo "FAIL: probe-value-42 did NOT materialize in the re-rendered config.h"
    echo "   the dump-vars rescue captured PROBE_RESULT but it did not"
    echo "   flow through the lifted configure_file's values namespace"
    echo "   --- re-rendered config.h ---"
    sed 's/^/   /' "$rendered"
    exit 1
fi

# Assertion (4): the broadened skip holds without the dump-vars
# capture too. With --dump-vars=false PROBE_RESULT is never captured
# into cmakeVars, yet the BucketProbe call still SKIPS (a
# host/toolchain probe is never a build input) — convert exits 0 with
# no Tier-1 refusal — and `probe-value-42` still materializes,
# recovered by the configure_file lift's byte-reverse-engineering of
# the (template, rendered) pair. Pins the broaden: an uncaptured probe
# is benign, not a refusal.
out_build_nodump="$work_dir/BUILD.bazel.nodump"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --lift-configure-file \
    --dump-vars=false \
    --out-build "$out_build_nodump" \
    >"$work_dir/convert.nodump.stdout" 2>"$work_dir/convert.nodump.stderr" || {
    echo "FAIL: convert-element-cmake (--dump-vars=false) exited non-zero"
    echo "   under the broaden an uncaptured host/toolchain probe should"
    echo "   SKIP (benign), not refuse Tier-1"
    sed 's/^/   stderr: /' "$work_dir/convert.nodump.stderr"
    exit 1
}
if grep -q 'unsupported-execute-process' "$work_dir/convert.nodump.stderr"; then
    echo "FAIL: unsupported-execute-process surfaced under --dump-vars=false"
    echo "   the broadened stance skips an uncaptured BucketProbe instead"
    echo "   of refusing it"
    sed 's/^/   stderr: /' "$work_dir/convert.nodump.stderr"
    exit 1
fi
if ! grep -q 'probe-value-42' "$out_build_nodump"; then
    echo "FAIL: probe-value-42 missing from the --dump-vars=false BUILD"
    echo "   the configure_file lift should byte-recover PROBE_RESULT from"
    echo "   the (template, rendered) pair even without the dump-vars capture"
    sed 's/^/   /' "$out_build_nodump"
    exit 1
fi

echo "ok  meta-cmake-execute-process-rescue: BucketProbe skips and the lifted configure_file re-renders probe-value-42 (via dump-vars capture, and under --dump-vars=false via byte-recovery)"
