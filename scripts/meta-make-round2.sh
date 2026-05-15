#!/bin/sh
# meta-make-round2.sh — render-half acceptance gate for kind:make
# joining the trace-driven round-2 architecture (the same
# architecture kind:autotools uses by default).
#
# kind:make opts into round-2 via pipelineHandler's
# traceDrivenSrckeyPatterns field (handler_make.go:makeSrckeyPatterns).
# When the operator passes --convert-element-trace +
# --build-tracer-bin + --trace-publish-bin + --trace-lookup-bin to
# write-a (and doesn't pass --trace-round1), kind:make elements
# render with the same round-2 shape as kind:autotools:
#
#   1. Project A's per-element BUILD is a converter genrule
#      consuming @trace_<elem>//:trace (the round-2 lookup repo
#      declared by rules/traces.bzl). Same shape as kind:autotools.
#   2. rules/traces.bzl + tools/traces.json render in both A and B,
#      with one entry per kind:make element.
#   3. Project B hosts the coarse install genrule (build-tracer
#      wrap + inline trace-publish). The legacy install-genrule-
#      in-A shape kind:make had before round-2 is gone — the
#      install genrule moved to B alongside the trace-publish step.
#
# Bazel-build half is intentionally out of scope here (the wire-
# level publish/lookup contract is unit-tested via cas.LocalStore
# in cmd/trace-{publish,lookup}/main_test.go; the bazel-side
# end-to-end against bb_clientd lives in
# tools/e2e-meta-autotools-round2-live.sh and is kind-agnostic).

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

fixture="testdata/meta-project/make-greet"

"$bin_dir/write-a" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup"

# A-side: converter genrule consuming :greet_trace_load.
a_build="$A/elements/greet/BUILD.bazel"
for marker in \
    'name = "greet_build"' \
    'load("//rules:traces.bzl", "trace_load")' \
    'name = "greet_trace_load"' \
    '":greet_trace_load"' \
    '"//tools:convert-element-trace"' \
    '--trace-dir' \
    '--out-build' \
    'kind:make round-2'; do
    if ! grep -qF -- "$marker" "$a_build"; then
        echo "meta-make-round2: A-side BUILD missing marker: $marker" >&2
        cat "$a_build" >&2
        exit 1
    fi
done

# A-side must NOT contain the legacy install genrule or the
# legacy load-time @trace_*//:trace external-repo label.
for banned in \
    'name = "greet_trace_build"' \
    '"@trace_greet//:trace"'; do
    if grep -qF -- "$banned" "$a_build"; then
        echo "meta-make-round2: A-side unexpectedly contains $banned" >&2
        cat "$a_build" >&2
        exit 1
    fi
done

# rules/traces.bzl renders in both projects. tools/traces.json is
# no longer emitted.
for path in \
    "$A/rules/traces.bzl" \
    "$B/rules/traces.bzl"; do
    if [ ! -f "$path" ]; then
        echo "meta-make-round2: missing $path" >&2
        exit 1
    fi
done
for path in \
    "$A/tools/traces.json" \
    "$B/tools/traces.json"; do
    if [ -f "$path" ]; then
        echo "meta-make-round2: $path unexpectedly emitted (legacy load-time wiring)" >&2
        exit 1
    fi
done

# A's MODULE.bazel must NOT declare the legacy `traces` extension.
mod_a="$A/MODULE.bazel"
for unwanted in \
    'use_extension("//rules:traces.bzl", "traces")' \
    '"trace_greet"'; do
    if grep -qF -- "$unwanted" "$mod_a"; then
        echo "meta-make-round2: A MODULE.bazel unexpectedly contains legacy traces extension wiring: $unwanted" >&2
        cat "$mod_a" >&2
        exit 1
    fi
done

# B-side: install genrule + trace-publish; converter is gone.
b_build="$B/elements/greet/BUILD.bazel"
for marker in \
    'name = "greet_trace_build"' \
    'tags = ["trace_build"]' \
    '"install_tree.tar"' \
    '"trace.log"' \
    '"make-db.txt"' \
    '"//tools:build-tracer"' \
    '"//tools:trace-publish"' \
    'CAS_GRPC_ADDR'; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-make-round2: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done
for banned in \
    '"BUILD.bazel.out"' \
    '"install-mapping.json"' \
    '//tools:convert-element-trace'; do
    if grep -qF -- "$banned" "$b_build"; then
        echo "meta-make-round2: B-side BUILD unexpectedly contains: $banned" >&2
        cat "$b_build" >&2
        exit 1
    fi
done

# Staged binaries — both projects host them so the //tools:X
# labels resolve from either project.
for path in \
    "$A/tools/trace-publish" \
    "$A/tools/trace-lookup" \
    "$B/tools/trace-publish" \
    "$B/tools/trace-lookup"; do
    if [ ! -x "$path" ]; then
        echo "meta-make-round2: missing executable $path" >&2
        exit 1
    fi
done

echo "meta-make-round2: render OK"
echo "meta-make-round2: ok"
