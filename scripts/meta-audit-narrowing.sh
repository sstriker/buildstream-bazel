#!/bin/sh
# meta-audit-narrowing.sh — end-to-end CI gate for the
# narrowing-undercoverage audit, soft-launch shape.
#
# Three-phase exercise:
#
#   1. Render a small meta-project with write-a. Project A
#      contains elements/<name>/srckey-patterns.txt (the
#      audit's input) and elements/<name>/srckey-expected-drift.txt
#      (the allowlist; empty for fixtures without per-element
#      drift declarations).
#   2. Populate the cmake oracle per kind:cmake element by
#      invoking convert-element offline against the element's
#      source tree with --out-cmake-configure-reads, writing
#      cmake-reads.json next to the element's pattern file.
#      For trace-driven kinds the trace oracle is empty (no
#      install-genrule build runs here); a CI variant that
#      exercises the trace oracle is queued for follow-up
#      once a build-tracer-on-CI fixture lands.
#   3. Run scripts/audit-narrowing-walk.sh against the
#      populated tree. The combined report is the gate's
#      signal — non-empty means drift; this v1 gate is
#      non-blocking (exit 0 either way) and surfaces the
#      report so reviewers can see the surface.
#
# Why hello-world: it's the smallest kind:cmake meta-project
# fixture in tree (one element, one .c, no configure_file or
# generated headers), so the audit's clean-case shape gets
# exercised without depending on a heavier multi-element
# meta-project. The gate's value here is the WIRING — that
# write-a stages the patterns + allowlist, that the walker
# discovers them, that audit-narrowing produces a report. The
# drift-detection logic itself is covered by
# cmd/audit-narrowing's unit tests.
#
# Non-blocking by design: the CI step that calls this script
# uses `continue-on-error: true` so a non-empty combined
# report doesn't fail the build. Once the allowlist has
# stabilized against a representative fixture set, the gate
# can be promoted to blocking (one-line CI change).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

# Build prereqs. convert-element is built by the Makefile's
# e2e-audit-narrowing target's `converter` dependency before
# this script runs, so the script doesn't repeat that build.
# write-a isn't part of `make converter`, so it needs its own
# go build. audit-narrowing itself is built inside
# scripts/audit-narrowing-walk.sh below.
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --bst testdata/meta-project/hello-world.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" >/dev/null

elements_dir="$A/elements"
if [ ! -d "$elements_dir" ]; then
    echo "meta-audit-narrowing: write-a did not produce $elements_dir" >&2
    exit 1
fi

# Populate cmake-reads.json for the kind:cmake element in this
# fixture. Hard-coded to hello-world today because the v1 gate
# uses that single-element fixture; growing to multi-element
# fixtures will require kind:cmake detection logic (probably
# walking write-a's emitted layout for elements that have an
# element-specific cmake reply / build.ninja sibling), which is
# queued behind real-world drift signal accumulation.
#
# convert-element runs cmake configure against the element's
# source root, emitting --out-cmake-configure-reads (the
# RERUN_CMAKE implicit-input projection) and a throwaway
# BUILD.bazel.out. We only care about the cmake-reads side
# here — the BUILD.bazel.out comes from the project A
# genrule path normally.
elem="hello-world"
elem_dir="$elements_dir/$elem"
if [ ! -f "$elem_dir/srckey-patterns.txt" ]; then
    echo "meta-audit-narrowing: expected $elem_dir/srckey-patterns.txt; write-a render shape changed?" >&2
    exit 1
fi

src_root="$(pwd)/testdata/meta-project/sources/hello-world"
throwaway_build="$work_dir/throwaway-BUILD.bazel"
convert_log="$work_dir/convert-element.log"
if ! "$bin_dir/convert-element" \
    --source-root="$src_root" \
    --out-build="$throwaway_build" \
    --out-cmake-configure-reads="$elem_dir/cmake-reads.json" \
    >"$convert_log" 2>&1; then
    # convert-element may fail on a fresh CI runner if cmake's
    # missing or the source tree's prereqs aren't installed.
    # Treat that as a non-blocking skip rather than a hard
    # fail — the audit gate's job is to surface drift when the
    # oracle CAN be captured, not to babysit the runner's
    # cmake availability. BUT surface the captured stdout +
    # stderr so a real regression (vs. just-missing-cmake) is
    # diagnosable from CI logs.
    echo "meta-audit-narrowing: convert-element failed for $elem (skipping audit); captured output follows:" >&2
    cat "$convert_log" >&2
    exit 0
fi

# Walk + accumulate.
"$repo_root/scripts/audit-narrowing-walk.sh" "$elements_dir"

combined="$elements_dir/audit-combined.txt"
if [ ! -f "$combined" ]; then
    echo "meta-audit-narrowing: walker did not produce $combined" >&2
    exit 1
fi

if [ -s "$combined" ]; then
    echo "meta-audit-narrowing: drift detected (soft signal — gate is non-blocking):" >&2
    cat "$combined" >&2
else
    echo "meta-audit-narrowing: clean (no drift across audited elements)" >&2
fi

# Exit 0 unconditionally — the report is the signal, not the
# exit status (matches cmd/audit-narrowing's own contract).
# The CI step's continue-on-error: true would also handle a
# non-zero exit, but keeping this script honest about its
# soft-gate role makes the contract explicit at call sites.
exit 0
