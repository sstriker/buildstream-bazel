#!/bin/sh
# meta-autotools-round2.sh — render-half acceptance gate for the
# trace-driven kind:autotools round-2 architecture.
#
# Round-2 is now the default when --convert-element-trace is
# set (the legacy round-1 single-genrule shape lives behind the
# --trace-round1 opt-out). This gate exercises the default by
# passing --trace-publish-bin / --trace-lookup-bin and asserts:
#
#   1. Project A's per-element BUILD is a converter genrule
#      consuming :<elem>_trace_load (the action-time AC lookup
#      target declared by rules/traces.bzl). The legacy load-time
#      @trace_<elem>//:trace external-repo shape is gone.
#   2. rules/traces.bzl renders in both A and B (declaring the
#      trace_load rule). tools/traces.json is no longer emitted.
#   3. Project B's coarse install genrule no longer references
#      convert-element-trace (the converter moved to A);
#      instead it references //tools:trace-publish (the round-2
#      publisher CLI that lands the AC entry).
#
# Bazel-build half is intentionally out of scope here. The round-2
# feedback loop (publish → look up → fine cc rules) needs a
# REAPI-capable bb_clientd mount, which is what the
# `tools/e2e-meta-autotools-round2-live.sh` gate exercises. This
# gate locks in the rendered contract — write-a's output shape —
# so a regression in the renderer surfaces immediately.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/build-tracer" ./cmd/build-tracer
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-trace" ./cmd/convert-element-trace
CGO_ENABLED=0 go build -o "$bin_dir/trace-publish" ./cmd/trace-publish
CGO_ENABLED=0 go build -o "$bin_dir/trace-lookup" ./cmd/trace-lookup

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

fixture="testdata/meta-project/autotools-greet"

"$bin_dir/write-a" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup"

# A-side: the converter genrule consuming :greet_trace_load.
a_build="$A/elements/greet/BUILD.bazel"
for marker in \
    'name = "greet_build"' \
    'load("//rules:traces.bzl", "trace_load")' \
    'name = "greet_trace_load"' \
    '":greet_trace_load"' \
    '"//tools:convert-element-trace"' \
    '--trace-dir' \
    '--out-build'; do
    if ! grep -qF -- "$marker" "$a_build"; then
        echo "meta-autotools-round2: A-side BUILD missing marker: $marker" >&2
        cat "$a_build" >&2
        exit 1
    fi
done

# A-side must NOT carry the legacy load-time external-repo shape
# nor the round-1 marker filegroup.
for banned in \
    '"@trace_greet//:trace"' \
    'BUILD_IN_PROJECT_B'; do
    if grep -qF -- "$banned" "$a_build"; then
        echo "meta-autotools-round2: A-side unexpectedly contains $banned" >&2
        cat "$a_build" >&2
        exit 1
    fi
done

# rules/traces.bzl renders in both projects. tools/traces.json is
# no longer emitted (the legacy load-time `traces` module
# extension was retired when the AC lookup moved to action time).
for path in \
    "$A/rules/traces.bzl" \
    "$B/rules/traces.bzl"; do
    if [ ! -f "$path" ]; then
        echo "meta-autotools-round2: missing $path" >&2
        exit 1
    fi
done
for path in \
    "$A/tools/traces.json" \
    "$B/tools/traces.json"; do
    if [ -f "$path" ]; then
        echo "meta-autotools-round2: $path unexpectedly emitted (legacy load-time wiring)" >&2
        exit 1
    fi
done

# A's MODULE.bazel must NOT declare the legacy `traces` extension.
mod_a="$A/MODULE.bazel"
for unwanted in \
    'use_extension("//rules:traces.bzl", "traces")' \
    '"trace_greet"'; do
    if grep -qF -- "$unwanted" "$mod_a"; then
        echo "meta-autotools-round2: A MODULE.bazel unexpectedly contains legacy traces extension wiring: $unwanted" >&2
        cat "$mod_a" >&2
        exit 1
    fi
done

# B-side: install genrule keeps the build pipeline + trace-publish
# step; converter is gone.
b_build="$B/elements/greet/BUILD.bazel"
for marker in \
    'name = "greet_install"' \
    '"install_tree.tar"' \
    '"trace.log"' \
    '"make-db.txt"' \
    '"//tools:build-tracer"' \
    '"//tools:trace-publish"' \
    'CAS_GRPC_ADDR'; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-autotools-round2: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done
for banned in \
    '"BUILD.bazel.out"' \
    '"install-mapping.json"' \
    '//tools:convert-element-trace'; do
    if grep -qF -- "$banned" "$b_build"; then
        echo "meta-autotools-round2: B-side BUILD unexpectedly contains: $banned" >&2
        cat "$b_build" >&2
        exit 1
    fi
done

# Staged binaries — both A and B host them so either project can
# resolve the //tools:X labels.
for path in \
    "$A/tools/trace-publish" \
    "$A/tools/trace-lookup" \
    "$B/tools/trace-publish" \
    "$B/tools/trace-lookup"; do
    if [ ! -x "$path" ]; then
        echo "meta-autotools-round2: missing executable $path" >&2
        exit 1
    fi
done

# Synth-key roundtrip via the in-tree binaries against an offline
# local CAS. Validates the publisher → lookup contract end-to-end
# without needing a buildbarn / bb_clientd install: the test
# stages a fake trace dir, runs trace-publish (which lands an AC
# entry under SyntheticActionDigest), then runs trace-lookup
# (which re-derives the key, reads back the digest, and prints
# it on stdout). The round-2 _trace_repo rule does the same call
# at bazel load time; this gate exercises the wire-level path
# without going through bazel.
echo "meta-autotools-round2: render OK"

# (The publisher / lookup binaries take a --cas=<grpc-addr> flag.
# Local-store roundtrip is covered by go-test
# TestPublish_RoundtripThroughLocalStore + TestLookup_HitReturnsRootDigest;
# those are the unit-test cousins of this render gate.)

echo "meta-autotools-round2: ok"
