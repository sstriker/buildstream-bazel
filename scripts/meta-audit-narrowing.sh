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
#      signal — non-empty means drift, and this script exits
#      non-zero so `make e2e-audit-narrowing` fails like any
#      other check target. The soft-vs-blocking dial lives in
#      the CI step (continue-on-error), not in this script's
#      exit-code policy.
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
# Exit shape: 0 on a clean audit, non-zero when the combined
# report is non-empty (drift detected) OR when any prereq
# (convert-element, the walker) failed. The soft-vs-blocking
# dial lives one level up in the CI step:
# `continue-on-error: true` keeps drift from failing the build
# while signal accumulates; flipping it to false promotes the
# gate to blocking. Keeping the policy decision in the CI
# workflow (rather than gated on a flag inside this script)
# makes the promotion a real one-line YAML change.

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
# Single-quote the trap body so $work_dir expands at trap-exec
# time (vs. trap-set time). The script keeps working either
# way today — work_dir is already in scope when this line
# runs — but the single-quoted idiom is the standard "shell
# trap takes a literal string to eval later" shape, and
# survives future refactors that might shift the trap above
# the assignment.
trap 'rm -rf "$work_dir"' EXIT

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
    # check-tools (the Makefile dep for e2e-audit-narrowing)
    # already validates the cmake / ninja / bwrap prereqs
    # this convert-element invocation needs, so a non-zero
    # exit here is a real regression — propagate it rather
    # than masking with an exit-0 skip. CI's
    # continue-on-error: true on the calling step preserves
    # the gate's soft-blocking shape; this script staying
    # honest about failures keeps the diagnostics actionable
    # (the captured stdout + stderr below is for the
    # operator's benefit, not for hiding the failure).
    echo "meta-audit-narrowing: convert-element failed for $elem; captured output follows:" >&2
    cat "$convert_log" >&2
    exit 1
fi

# Walk + accumulate.
"$repo_root/scripts/audit-narrowing-walk.sh" "$elements_dir"

combined="$elements_dir/audit-combined.txt"
if [ ! -f "$combined" ]; then
    echo "meta-audit-narrowing: walker did not produce $combined" >&2
    exit 1
fi

if [ -s "$combined" ]; then
    echo "meta-audit-narrowing: drift detected:" >&2
    cat "$combined" >&2
    # Exit non-zero on drift so the soft-vs-blocking dial is
    # entirely a property of the CI step (continue-on-error)
    # rather than this script. The underlying primitives
    # (cmd/audit-narrowing, scripts/audit-narrowing-walk.sh)
    # still follow the "exit 0, report is the signal" pattern
    # because they're policy-agnostic; this meta script IS
    # the policy layer (it bundles the gate's render + oracle-
    # populate + walk + report shape into one CI-callable
    # surface), so non-zero on drift here makes
    # "flip continue-on-error to false" a real promotion
    # gesture rather than a misleading docs claim.
    exit 1
fi

echo "meta-audit-narrowing: clean (no drift across audited elements)" >&2
exit 0
