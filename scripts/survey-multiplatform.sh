#!/bin/sh
# survey-multiplatform.sh — survey a CMake project across SEVERAL target
# platforms and fold the per-platform IRs into one BUILD with real
# `select()` arms, making the platform/arch intent observable.
#
# Why: cmake's File API codemodel describes only the platform you
# configured on — an `if(WIN32) target_sources(... win32.c)` branch is
# invisible to a Linux configure. The lower-side #217 trace partition
# recovers SOME of the other arms from a single configure's trace, but
# the authoritative way to capture every platform's sources is to
# CONFIGURE PER PLATFORM and fold. This script does that:
#
#   for each platform P in {linux(native), windows, darwin}:
#     convert-element-cmake --source-root <src> [--toolchain <P>.cmake]
#       --out-ir-json <P>/ir.json
#   fold-element --cell linux|... --cell windows|... --cell darwin|...
#       --out-build <out>/BUILD.bazel
#
# The non-native platforms use synthetic toolchain files
# (scripts/survey-toolchains/<os>.cmake) that set CMAKE_SYSTEM_NAME and
# force the compiler check — cmake evaluates the platform if() branches
# and emits a codemodel WITHOUT needing a real cross-compiler (the
# artefacts are never built; only the File API reply + trace are used).
#
# Usage:
#   scripts/survey-multiplatform.sh <name>=<src> [<name>=<src> ...]
#
# Env:
#   SURVEY_MP_PLATFORMS  space list of platforms to fold (default
#                        "linux windows darwin"; linux is the native
#                        configure, the rest use survey-toolchains/).
#   SURVEY_MP_OUT_DIR    output dir (default /tmp/survey-mp-out).
#
# Output per project: <out>/<name>/<platform>/ir.json, the folded
# <out>/<name>/BUILD.bazel, and a summary line reporting how many targets
# gained a platform select() (the multi-platform signal).
set -eu

if [ "$#" -lt 1 ]; then
    echo "usage: $0 <name>=<src> [<name>=<src> ...]" >&2
    exit 2
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

platforms="${SURVEY_MP_PLATFORMS:-linux windows darwin}"
out_dir="${SURVEY_MP_OUT_DIR:-/tmp/survey-mp-out}"
toolchain_dir="$repo_root/scripts/survey-toolchains"

# cmake/ninja are required (the converter runs cmake itself). Skip
# cleanly when absent — same contract as the meta-* gates.
for tool in cmake ninja; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "survey-multiplatform: $tool not on PATH; skipping (not a failure)"
        exit 0
    fi
done

bin_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir"' EXIT
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake
CGO_ENABLED=0 go build -o "$bin_dir/fold-element" ./converter/cmd/fold-element

mkdir -p "$out_dir"
summary="$out_dir/summary.txt"
: > "$summary"
printf '%-14s %10s %12s %s\n' project platforms select_targets status | tee "$summary"
printf '%-14s %10s %12s %s\n' ------- --------- -------------- ------ | tee -a "$summary"

# constraints_for <platform> — echo the @platforms//os:* constraint the
# fold keys this platform's deltas under.
constraints_for() {
    case "$1" in
        linux)   echo "@platforms//os:linux" ;;
        windows) echo "@platforms//os:windows" ;;
        darwin)  echo "@platforms//os:darwin" ;;
        *)       echo "" ;;
    esac
}

for entry in "$@"; do
    name="${entry%%=*}"
    src="${entry#*=}"
    if [ "$name" = "$entry" ] || [ -z "$src" ]; then
        echo "survey-multiplatform: bad spec '$entry' (want <name>=<src>)" >&2
        continue
    fi
    proj_out="$out_dir/$name"
    mkdir -p "$proj_out"
    if [ ! -d "$src" ]; then
        printf '%-14s %10s %12s %s\n' "$name" - - "MISSING ($src)" | tee -a "$summary"
        continue
    fi

    # Convert one cell per platform. A platform whose configure fails
    # (e.g. a project that hard-requires a real cross-compiler) is
    # dropped from the matrix — the fold still runs over the platforms
    # that succeeded.
    cell_args=""
    ok_platforms=""
    for p in $platforms; do
        constraint="$(constraints_for "$p")"
        if [ -z "$constraint" ]; then
            echo "  $name: unknown platform '$p'; skipping" >&2
            continue
        fi
        cell_out="$proj_out/$p"
        mkdir -p "$cell_out"
        tc_arg=""
        if [ "$p" != "linux" ]; then
            tc_file="$toolchain_dir/$p.cmake"
            if [ ! -f "$tc_file" ]; then
                echo "  $name: no toolchain file $tc_file; skipping $p" >&2
                continue
            fi
            tc_arg="--toolchain-cmake-file=$tc_file"
        fi
        # shellcheck disable=SC2086 # tc_arg is intentionally word-split (empty or one flag).
        if "$bin_dir/convert-element-cmake" \
            --source-root "$src" \
            --diagnostics \
            $tc_arg \
            --out-ir-json "$cell_out/ir.json" \
            --out-build "$cell_out/BUILD.bazel" \
            > "$cell_out/convert.log" 2>&1
        then
            cell_args="$cell_args --cell $p|$constraint|$cell_out/ir.json"
            ok_platforms="$ok_platforms $p"
        else
            echo "  $name: $p configure/convert failed (see $cell_out/convert.log); dropping from matrix" >&2
        fi
    done

    n_ok="$(printf '%s' "$ok_platforms" | wc -w | tr -d ' ')"
    if [ "$n_ok" -lt 1 ]; then
        printf '%-14s %10s %12s %s\n' "$name" 0 - "ALL PLATFORMS FAILED" | tee -a "$summary"
        continue
    fi

    # Fold the cells. With a single surviving cell the fold is identity
    # (PerPlatform passes through), which is still correct.
    # shellcheck disable=SC2086 # cell_args is an intentional argv list.
    if "$bin_dir/fold-element" \
        $cell_args \
        --out-build "$proj_out/BUILD.bazel" \
        > "$proj_out/fold.log" 2>&1
    then
        # Count targets that gained a platform select() — the signal that
        # multi-platform intent landed (rules whose srcs/deps/etc. carry
        # `+ select({@platforms//...})`).
        sel="$(grep -c 'select({' "$proj_out/BUILD.bazel" 2>/dev/null | head -1)"
        [ -n "$sel" ] || sel=0
        printf '%-14s %10s %12s %s\n' "$name" "$n_ok" "$sel" "ok" | tee -a "$summary"
    else
        printf '%-14s %10s %12s %s\n' "$name" "$n_ok" - "FOLD FAILED — see $proj_out/fold.log" | tee -a "$summary"
    fi
done

echo ""
echo "Per project: $out_dir/<name>/{<platform>/ir.json, BUILD.bazel}; summary at $summary"
