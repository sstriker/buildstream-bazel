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
#   SURVEY_MP_LIFT_OPTIONS  comma list of cmake option names to lift
#                        (--lift-options) in EACH per-platform convert;
#                        option arms fold through elementfold (agreeing
#                        arms pass through, platform-conditional arms
#                        become selects.config_setting_group AND-arms).
#                        The //options package is taken from the FIRST
#                        surviving platform's convert (a bool_flag /
#                        string_flag default can't vary per platform —
#                        option(FOO ... \${WIN32})-style platform-
#                        dependent defaults keep the first platform's);
#                        a divergence between platforms' //options
#                        packages is surfaced as a warning.
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

# "auto" (the default) derives each project's platform set from its own
# if()/elseif() predicates via a native trace configure (detect_platforms);
# an explicit space-list (e.g. "linux windows") forces that set for every
# project.
platforms="${SURVEY_MP_PLATFORMS:-auto}"
out_dir="${SURVEY_MP_OUT_DIR:-/tmp/survey-mp-out}"
lift_options="${SURVEY_MP_LIFT_OPTIONS:-}"
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

# detect_platforms <src> — derive the platform matrix from the project's
# OWN platform if()/elseif() predicates, recovered from a single native
# --trace-expand configure. cmake records every if/elseif predicate it
# EVALUATES — including the branches it didn't take — so a Linux configure
# still sees `if(WIN32)` / `elseif(APPLE)` etc. We parse the project-file
# (non-cmake-module) predicates and map the recognized platform shorthands
# to our matrix names; native `linux` is always included (it's the
# configure host). Echoes a space-separated set; empty configure → just
# "linux". Mirrors the converter's selectKeyFromIfArgs recognizer.
detect_platforms() {
    _dp_src="$1"
    _dp_bld="$(mktemp -d)"
    cmake -S "$_dp_src" -B "$_dp_bld" -G Ninja \
        --trace-expand --trace-format=json-v1 \
        --trace-redirect="$_dp_bld/trace.jsonl" >/dev/null 2>&1 || true
    _dp_set="linux"
    if [ -f "$_dp_bld/trace.jsonl" ] && command -v python3 >/dev/null 2>&1; then
        _dp_extra="$(python3 - "$_dp_bld/trace.jsonl" <<'PY'
import json, sys
recog = {
    "WIN32": "windows", "MSVC": "windows", "MINGW": "windows", "CYGWIN": "windows",
    "APPLE": "darwin", "LINUX": "linux",
}
sysname = {"Windows": "windows", "Darwin": "darwin", "Linux": "linux"}
out = set()
for line in open(sys.argv[1], errors="ignore"):
    try:
        e = json.loads(line)
    except Exception:
        continue
    if e.get("cmd") not in ("if", "elseif"):
        continue
    f = e.get("file", "")
    # project files only — skip cmake's own modules (they probe the HOST,
    # not the project's intended target platforms).
    if "/Modules/" in f or "/share/cmake" in f or "/cmake-build" in f:
        continue
    args = e.get("args", [])
    for a in args:
        if a in recog:
            out.add(recog[a])
    if len(args) >= 3 and args[0] == "CMAKE_SYSTEM_NAME" and args[1] in ("STREQUAL", "MATCHES"):
        nm = args[2].strip('"')
        if nm in sysname:
            out.add(sysname[nm])
print(" ".join(sorted(out)))
PY
)"
        for _p in $_dp_extra; do
            case " $_dp_set " in *" $_p "*) ;; *) _dp_set="$_dp_set $_p" ;; esac
        done
    fi
    rm -rf "$_dp_bld"
    printf '%s' "$_dp_set"
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

    # Resolve this project's platform set: "auto" derives it from the
    # project's own if()/elseif() predicates (one native trace configure);
    # an explicit list is used verbatim.
    proj_platforms="$platforms"
    if [ "$platforms" = "auto" ]; then
        proj_platforms="$(detect_platforms "$src")"
        echo "  $name: platforms (auto-detected): $proj_platforms" >&2
    fi

    # Convert one cell per platform. A platform whose configure fails
    # (e.g. a project that hard-requires a real cross-compiler) is
    # dropped from the matrix — the fold still runs over the platforms
    # that succeeded.
    cell_args=""
    ok_platforms=""
    for p in $proj_platforms; do
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
        lift_args=""
        if [ -n "$lift_options" ]; then
            lift_args="--lift-options=$lift_options --out-option-settings=$cell_out/options-BUILD.bazel"
        fi
        # shellcheck disable=SC2086 # tc_arg/lift_args are intentionally word-split.
        if "$bin_dir/convert-element-cmake" \
            --source-root "$src" \
            --diagnostics \
            $tc_arg \
            $lift_args \
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
        # Stage the //options package from the first surviving cell (flag
        # defaults are per-convert; they can't vary per platform), warning
        # when another cell's package diverges — the operator picks which
        # platform's defaults win.
        if [ -n "$lift_options" ]; then
            first_opts=""
            for p in $ok_platforms; do
                cell_opts="$proj_out/$p/options-BUILD.bazel"
                [ -f "$cell_opts" ] || continue
                if [ -z "$first_opts" ]; then
                    first_opts="$cell_opts"
                    mkdir -p "$proj_out/options"
                    cp "$cell_opts" "$proj_out/options/BUILD.bazel"
                elif ! cmp -s "$first_opts" "$cell_opts"; then
                    echo "  $name: //options package differs between platforms ($first_opts vs $cell_opts) — platform-dependent option defaults keep the first platform's; review before staging" >&2
                fi
            done
        fi
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
