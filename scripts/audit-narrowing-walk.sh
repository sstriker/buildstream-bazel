#!/bin/sh
# audit-narrowing-walk.sh — run cmd/audit-narrowing against every
# element under an artifact tree, accumulating per-element
# undercoverage reports into one combined output.
#
# Expected artifact layout (the populator's contract):
#
#   <artifact-dir>/
#       <elem-name>/
#           srckey-patterns.txt          # required for inclusion
#           srckey-expected-drift.txt    # optional (passed as --allowlist)
#           cmake-reads.json             # optional (cmake oracle)
#           trace.log                    # optional (trace oracle)
#           audit-report.txt             # written by this script
#
# Per-element behavior:
#   - srckey-patterns.txt absent → element skipped silently
#     (e.g. a stack/compose element with nothing to audit).
#   - Neither oracle present → element skipped silently with a
#     stderr note (the audit needs at least one oracle; an
#     element opted in for trace-source-root but without the
#     trace.log captured yet falls in this bucket).
#   - Either oracle present → run audit-narrowing, write
#     audit-report.txt next to the patterns file. Empty report
#     is the clean signal.
#
# Exit status is always 0; the combined report (one
# `<elem>: <path>` line per drift entry, sorted) is the
# soft-gate signal. CI gates that want hard-fail-on-drift can
# `[ ! -s combined-report.txt ]` and fail when it isn't empty
# — the gate's blocking decision lives one level up, not in
# this walker.
#
# Usage:
#   audit-narrowing-walk.sh <artifact-dir> [<combined-report-path>]
#
# combined-report-path defaults to <artifact-dir>/audit-combined.txt.

set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
    echo "usage: $0 <artifact-dir> [<combined-report-path>]" >&2
    exit 2
fi

artifact_dir="$1"
combined_report="${2:-$artifact_dir/audit-combined.txt}"

if [ ! -d "$artifact_dir" ]; then
    echo "audit-narrowing-walk: $artifact_dir is not a directory" >&2
    exit 2
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/audit-narrowing" ./cmd/audit-narrowing

: > "$combined_report"

# Per-element walk. The shell `for` loop expands the glob in
# the parent shell, so an empty artifact tree (no element
# subdirs) skips cleanly. find -mindepth 1 -maxdepth 1 -type d
# would also work but is harder to read.
for elem_path in "$artifact_dir"/*/; do
    # Trailing slash guards against the glob's no-match case
    # in shells that leave the literal pattern when nothing
    # expands; "for" iterates zero times in that case here too,
    # but we double-check the path is a directory.
    [ -d "$elem_path" ] || continue
    elem_name="$(basename "$elem_path")"

    patterns="$elem_path/srckey-patterns.txt"
    if [ ! -f "$patterns" ]; then
        # No patterns → no audit input. Stack / compose / filter
        # elements land here; skip silently.
        continue
    fi

    cmake_reads="$elem_path/cmake-reads.json"
    trace_log="$elem_path/trace.log"
    drift_file="$elem_path/srckey-expected-drift.txt"
    report="$elem_path/audit-report.txt"

    # Build optional flags as positional args via `set --` so
    # each flag stays a single argv entry even when its embedded
    # path contains spaces. Concatenating + relying on word-
    # splitting would break for any artifact tree under a path
    # like `/Users/Alice Doe/...` — the audit binary would see
    # `--cmake-reads=/Users/Alice` as one arg and `Doe/...` as
    # another.
    set --
    have_oracle=0
    if [ -f "$cmake_reads" ]; then
        set -- "$@" "--cmake-reads=$cmake_reads"
        have_oracle=1
    fi
    if [ -f "$trace_log" ]; then
        set -- "$@" "--trace=$trace_log"
        have_oracle=1
    fi
    if [ "$have_oracle" -eq 0 ]; then
        echo "audit-narrowing-walk: $elem_name has srckey-patterns.txt but no oracle (cmake-reads.json or trace.log); skipping" >&2
        continue
    fi
    if [ -f "$drift_file" ]; then
        set -- "$@" "--allowlist=$drift_file"
    fi

    "$bin_dir/audit-narrowing" \
        --patterns="$patterns" \
        --out="$report" \
        "$@"

    # Project the per-element report into the combined report
    # with the elem name as a prefix so consumers don't have
    # to grep the elements/ tree.
    if [ -s "$report" ]; then
        while IFS= read -r line; do
            printf '%s: %s\n' "$elem_name" "$line" >> "$combined_report"
        done < "$report"
    fi
done

# Report the combined drift count to stderr for run-time
# visibility (the report file itself is the canonical surface;
# this is just a friendly summary). wc -l on an empty file
# emits "0", so the unconditional message is informative even
# in the clean case.
drift_count="$(wc -l < "$combined_report" | tr -d ' ')"
echo "audit-narrowing-walk: $drift_count drift entries under $artifact_dir" >&2
echo "audit-narrowing-walk: combined report at $combined_report" >&2
