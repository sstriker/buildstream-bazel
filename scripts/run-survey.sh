#!/bin/sh
# run-survey.sh — drive the diagnostic-survey corpus through the cmake
# converter and emit a per-project rejection + bazel-idiom report.
#
# This is the self-contained, in-repo version of the cross-project
# survey described in docs/codemodel-consumption-audit.md. It does NOT
# depend on any external harness: convert-element-cmake is self-driving
# in --source-root mode (it runs cmake itself with the File API + trace
# hooks), and --diagnostics collects every Tier-1 refusal and continues
# past it rather than aborting on the first.
#
# Each project is converted into a scratch out dir; the emitted
# BUILD.bazel is best-effort and NOT guaranteed to build — the point is
# enumerating the refusal + idiom surface in one pass, not producing
# usable output.
#
# Usage:
#   scripts/run-survey.sh [--out-dir <dir>] [name=<src-root> ...]
#
# With no project args it surveys the four corpus projects at their
# Makefile-pinned clone dirs (run `make fetch-survey` first):
#   abseil=$ABSEIL_DIR protobuf=$PROTOBUF_DIR
#   googletest=$GTEST_DIR eigen=$EIGEN_DIR
#
# Env / defaults mirror the Makefile vars; override to point elsewhere.
set -eu

out_dir="${SURVEY_OUT_DIR:-/tmp/survey-out}"
projects=""

while [ $# -gt 0 ]; do
    case "$1" in
        --out-dir) out_dir="$2"; shift 2 ;;
        -h|--help)
            sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *=*) projects="$projects $1"; shift ;;
        *) echo "run-survey.sh: unrecognized arg '$1'" >&2; exit 2 ;;
    esac
done

# Default corpus: the four pinned in the Makefile.
if [ -z "$projects" ]; then
    projects="
        abseil=${ABSEIL_DIR:-/tmp/abseil-cpp}
        protobuf=${PROTOBUF_DIR:-/tmp/protobuf}
        googletest=${GTEST_DIR:-/tmp/googletest}
        eigen=${EIGEN_DIR:-/tmp/eigen}
    "
fi

# Locate the converter: prefer the Makefile-built binary, fall back to
# `go run` so the script works in a bare checkout.
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
converter="$repo_root/build/bin/convert-element-cmake"
if [ -x "$converter" ]; then
    run_converter() { "$converter" "$@"; }
else
    echo "note: $converter not built; using 'go run' (slower). Run 'make converter' to speed up." >&2
    run_converter() { ( cd "$repo_root" && go run ./converter/cmd/convert-element-cmake "$@" ); }
fi

mkdir -p "$out_dir"
summary="$out_dir/summary.txt"
: > "$summary"

printf '%-14s %10s %10s %s\n' project rejections idioms status | tee "$summary"
printf '%-14s %10s %10s %s\n' ------- ---------- ------ ------ | tee -a "$summary"

for entry in $projects; do
    name="${entry%%=*}"
    src="${entry#*=}"
    proj_out="$out_dir/$name"
    mkdir -p "$proj_out"

    if [ ! -d "$src" ]; then
        printf '%-14s %10s %10s %s\n' "$name" - - "MISSING ($src) — run 'make fetch-$name'" | tee -a "$summary"
        continue
    fi

    rej="$proj_out/rejections.json"
    idiom="$proj_out/bazel-idiom.json"
    status="ok"

    # The survey runs each project standalone, so out-of-tree
    # find_package(...) deps (e.g. protobuf's find_package(ZLIB))
    # surface as honest find-package-dep-unresolved findings. In a
    # real .bst element graph these resolve through the orchestrated
    # producer→consumer export channel (write-a stages each kind:cmake
    # dep's exports.json + cmake-config bundle into the consumer's
    # convert genrule); the standalone survey deliberately doesn't
    # paper over the gap with a hand-authored imports manifest.

    # --diagnostics implies --ignore-rejections-for-diagnostics; the run
    # continues past refusals. A non-zero exit here means cmake configure
    # itself failed (itself a survey datapoint), not a refusal.
    if ! run_converter \
        --source-root "$src" \
        --diagnostics \
        --rejections-report "$rej" \
        --audit-bazel-idiom-report "$idiom" \
        --out-build "$proj_out/BUILD.bazel" \
        --out-failure "$proj_out/failure.json" \
        > "$proj_out/convert.log" 2>&1
    then
        status="CONFIGURE/CONVERT FAILED — see $proj_out/convert.log"
    fi

    # Count records. The rejections report is a JSON array of
    # {code,...}; the bazel-idiom report is an array of {Code,...}
    # (capitalised — distinct Go structs).
    rej_n=$( [ -f "$rej" ]   && grep -o '"code"' "$rej"  2>/dev/null | wc -l | tr -d ' ' || echo "-" )
    idi_n=$( [ -f "$idiom" ] && grep -o '"Code"' "$idiom" 2>/dev/null | wc -l | tr -d ' ' || echo "-" )

    printf '%-14s %10s %10s %s\n' "$name" "${rej_n:--}" "${idi_n:--}" "$status" | tee -a "$summary"
done

echo ""
echo "Reports under $out_dir/<project>/{rejections,bazel-idiom}.json; summary at $summary"
