#!/bin/sh
# meta-cmake-interface-genex-defines.sh — render gate for the
# INTERFACE_COMPILE_DEFINITIONS structural-probe reconciliation.
#
# An INTERFACE library's config-gated define
# ($<$<CONFIG:Release>:RELEASE_ONLY=1>) is a generator expression
# the Go-side (a) evaluator can't crack (its lexer rejects the
# nested `$<`). cmake's File API also omits INTERFACE_LIBRARY
# targets from its codemodel targets[], so the converter
# synthesizes the cc_library from the trace — and before this fix,
# silently DROPPED the unresolvable define, losing intent that
# genuinely applies under the configured build.
#
# With --probe-genex (default ON), the structural genex probe
# captures cmake's own resolved INTERFACE_COMPILE_DEFINITIONS for
# the configured build type. buildGenexTargets now folds the
# INTERFACE_LIBRARY probe (an exception to the "codemodel is ground
# truth" skip), and lowerInterfaceLibraries reconciles the dropped
# define against it — emitting RELEASE_ONLY=1 instead of losing it.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/interface-genex-defines.
#
# Asserts:
#   1. With --probe-genex, the synthesized `iface` cc_library
#      carries BOTH PLAIN_DEF=1 and RELEASE_ONLY=1 (the configured
#      build is Release by default) — the genex define was resolved
#      from the probe, not dropped.
#   2. Negative / load-bearing: with --probe-genex=false the same
#      define is dropped (only PLAIN_DEF=1 survives), pinning the
#      structural probe as the thing that recovers it — so
#      assertion 1 can't pass vacuously.
#
# cmake 3.24+ is required for the probe hook; the gate skips
# cleanly when cmake isn't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/interface-genex-defines"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

# Extract the defines block of the `iface` cc_library.
iface_block() {
    awk '/name = "iface"/,/^\)/' "$1"
}

# --- Assertion 1: --probe-genex recovers the config-gated define. ---
out_on="$work_dir/BUILD.on"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_on" \
    --probe-genex=true \
    >"$work_dir/on.stdout" 2>"$work_dir/on.stderr" || {
    echo "FAIL: convert-element-cmake (--probe-genex) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/on.stderr"
    exit 1
}

blk_on="$(iface_block "$out_on")"
if ! printf '%s\n' "$blk_on" | grep -q 'PLAIN_DEF=1'; then
    echo "FAIL: iface cc_library missing PLAIN_DEF=1"
    printf '%s\n' "$blk_on" | sed 's/^/   /'
    exit 1
fi
if ! printf '%s\n' "$blk_on" | grep -q 'RELEASE_ONLY=1'; then
    echo "FAIL: iface cc_library missing RELEASE_ONLY=1 — the config-gated"
    echo "   genex define was dropped instead of recovered from the probe"
    printf '%s\n' "$blk_on" | sed 's/^/   /'
    exit 1
fi

# --- Assertion 2 (load-bearing): without the probe, the define drops. ---
out_off="$work_dir/BUILD.off"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_off" \
    --probe-genex=false \
    >"$work_dir/off.stdout" 2>"$work_dir/off.stderr" || {
    echo "FAIL: convert-element-cmake (--probe-genex=false) exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/off.stderr"
    exit 1
}

blk_off="$(iface_block "$out_off")"
if printf '%s\n' "$blk_off" | grep -q 'RELEASE_ONLY=1'; then
    echo "FAIL: --probe-genex=false still emitted RELEASE_ONLY=1"
    echo "   without the probe the unresolvable genex define must drop —"
    echo "   assertion 1 would otherwise be passing vacuously"
    printf '%s\n' "$blk_off" | sed 's/^/   /'
    exit 1
fi
if ! printf '%s\n' "$blk_off" | grep -q 'PLAIN_DEF=1'; then
    echo "FAIL: --probe-genex=false dropped PLAIN_DEF=1 too (expected it to survive)"
    printf '%s\n' "$blk_off" | sed 's/^/   /'
    exit 1
fi

echo "ok  meta-cmake-interface-genex-defines: config-gated INTERFACE define (\$<\$<CONFIG:Release>:RELEASE_ONLY=1>) recovered from the structural probe instead of dropped; load-bearing under --probe-genex=false"
