#!/bin/sh
# meta-cmake-standalone-custom-command.sh — render gate for the
# Phase 4 standalone-genrule emission graduation.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/standalone-custom-command,
# a CMakeLists with an add_custom_command whose output is consumed
# only by an add_custom_target. The recoverGenrule path covers
# custom-command edges whose outputs other cmake targets reference
# as sources; this fixture exercises the standalone shape — a
# custom command whose output no cc target consumes. The Phase 4
# walker is the only path that emits a genrule for this edge.
#
# Phase 4 graduation flips --emit-standalone-custom-commands from
# off-by-default to on-by-default; this gate runs convert-element-
# cmake without any explicit `--emit-standalone-custom-commands`
# argument and asserts the genrule still surfaces. The default-on
# behaviour is the operator-facing contract.
#
# Asserts:
#   1. convert-element-cmake exits 0.
#   2. The generated BUILD.bazel contains a genrule whose `outs`
#      reference `standalone_stamp.txt` — proves the standalone
#      walker fired without the opt-in flag.
#   3. The genrule carries the `cmake-codegen-standalone-custom-
#      command` tag so the Phase 7 audit can inventory it.
#   4. The companion cc_library (`stub`) survives unchanged —
#      flipping the standalone walker's default doesn't perturb
#      the normal target emission.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi

# Build the converter so the gate has a binary to drive.
bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/standalone-custom-command"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

# No --emit-standalone-custom-commands flag — the Phase 4
# graduation flips it on by default. If the default ever
# regresses to off, this gate fails by missing the genrule.
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

# The standalone walker emits a genrule named after the sanitized
# first output. For `standalone_stamp.txt` this comes out as
# `custom_command_standalone_stamp_txt`. Assert the name and the
# outs reference together to pin both the naming convention and
# the wiring.
if ! grep -q 'name = "custom_command_standalone_stamp_txt"' "$out_build"; then
    echo "FAIL: standalone genrule missing from BUILD.bazel"
    echo "   expected a genrule named custom_command_standalone_stamp_txt"
    echo "   default-on --emit-standalone-custom-commands appears regressed"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
fi

if ! grep -q 'standalone_stamp.txt' "$out_build"; then
    echo "FAIL: outs reference to standalone_stamp.txt missing"
    sed 's/^/   /' "$out_build"
    exit 1
fi

if ! grep -q 'cmake-codegen-standalone-custom-command' "$out_build"; then
    echo "FAIL: cmake-codegen-standalone-custom-command tag missing"
    echo "   the Phase 7 audit needs the tag to inventory the emission"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# The companion cc_library should still emit as before.
if ! grep -q 'name = "stub"' "$out_build"; then
    echo "FAIL: companion cc_library 'stub' missing — emission regressed"
    sed 's/^/   /' "$out_build"
    exit 1
fi

# Confirm the opt-out path still works: passing
# --emit-standalone-custom-commands=false should suppress the
# walker's output. Operators who hit edge cases need the escape
# hatch.
out_build_opt_out="$work_dir/BUILD.bazel.opt-out"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --emit-standalone-custom-commands=false \
    --out-build "$out_build_opt_out" \
    >"$work_dir/convert.opt-out.stdout" 2>"$work_dir/convert.opt-out.stderr" || {
    echo "FAIL: --emit-standalone-custom-commands=false (opt-out) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.opt-out.stderr"
    exit 1
}
if grep -q 'cmake-codegen-standalone-custom-command' "$out_build_opt_out"; then
    echo "FAIL: opt-out (--emit-standalone-custom-commands=false) still emitted the standalone genrule"
    sed 's/^/   /' "$out_build_opt_out"
    exit 1
fi

echo "ok  meta-cmake-standalone-custom-command: Phase 4 standalone-genrule emission default-on, opt-out honoured"
