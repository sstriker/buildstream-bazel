#!/bin/sh
# meta-cmake-round2-fallback.sh — render-half acceptance gate
# for kind:cmake's Phase B execute_process round-2 fallback
# shape. See docs/design/cmake-execute-process-round2-fallback.md
# for the full architecture.
#
# When the operator passes --cmake-round2-fallback +
# --build-tracer-bin + --trace-publish-bin to write-a, every
# kind:cmake element renders with:
#
#   1. Project A's per-element converter genrule threads
#      --unsupported-execute-process-fallback=true into
#      convert-element-cmake so classifier refusals on
#      execute_process produce the placeholder shape (per-target
#      cc_import / sh_binary stubs + extract genrule pointing
#      at install_tree.tar) instead of exiting Tier-1.
#   2. Project B's per-element BUILD emits a real install
#      genrule wrapping cmake configure + ninja + install + tar
#      under build-tracer, plus inline trace-publish (when
#      CAS_GRPC_ADDR is set in the action env).
#   3. build-tracer + trace-publish stage into both projects'
#      tools/ so the //tools:X labels resolve from either side.
#
# The Bazel-build half is intentionally out of scope here (the
# wire-level publish/lookup contract is unit-tested via
# cas.LocalStore in cmd/trace-{publish,lookup}/main_test.go;
# the bazel-side end-to-end is queued behind the trace-driven
# convergence research follow-on, since v1 doesn't yet wire
# A's load-time @trace_<elem>//:trace lookup).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/build-tracer" ./cmd/build-tracer
CGO_ENABLED=0 go build -o "$bin_dir/trace-publish" ./cmd/trace-publish
CGO_ENABLED=0 go build -o "$bin_dir/trace-lookup" ./cmd/trace-lookup

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

# Reuse the existing hello-world fixture — kind:cmake element
# whose round-2-fallback rendered shape we want to assert.
fixture="testdata/meta-project"

"$bin_dir/write-a" \
    --bst "$fixture/hello-world.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup" \
    --cmake-round2-fallback

# A-side: converter genrule threads the fallback flag AND pulls
# :hello-world_trace_load into srcs (the action-time AC lookup;
# trace-driven convergence research follow-on teaches
# convert-element-cmake to consume the trace bytes).
a_build="$A/elements/hello-world/BUILD.bazel"
for marker in \
    '--unsupported-execute-process-fallback=true' \
    '"//tools:convert-element-cmake"' \
    'load("//rules:traces.bzl", "trace_load")' \
    'name = "hello-world_trace_load"' \
    'expect_make_db = False' \
    '":hello-world_trace_load"'; do
    if ! grep -qF -- "$marker" "$a_build"; then
        echo "meta-cmake-round2-fallback: A-side BUILD missing marker: $marker" >&2
        cat "$a_build" >&2
        exit 1
    fi
done
# Legacy external-repo trace label must NOT appear — the
# action-time trace_load is in the same package.
if grep -qF -- '"@trace_hello-world//:trace"' "$a_build"; then
    echo "meta-cmake-round2-fallback: A-side BUILD unexpectedly contains legacy @trace_*//:trace label" >&2
    cat "$a_build" >&2
    exit 1
fi

# rules/traces.bzl renders in both projects. tools/traces.json is
# no longer emitted (the legacy load-time `traces` module extension
# was retired when the AC lookup moved from load time to action
# time).
for path in \
    "$A/rules/traces.bzl" \
    "$B/rules/traces.bzl"; do
    if [ ! -f "$path" ]; then
        echo "meta-cmake-round2-fallback: missing $path" >&2
        exit 1
    fi
done
for path in \
    "$A/tools/traces.json" \
    "$B/tools/traces.json"; do
    if [ -f "$path" ]; then
        echo "meta-cmake-round2-fallback: $path unexpectedly emitted (legacy load-time wiring)" >&2
        exit 1
    fi
done

# A's MODULE.bazel must NOT declare the legacy `traces` extension.
mod_a="$A/MODULE.bazel"
for unwanted in \
    'use_extension("//rules:traces.bzl", "traces")' \
    '"trace_hello-world"'; do
    if grep -qF -- "$unwanted" "$mod_a"; then
        echo "meta-cmake-round2-fallback: A MODULE.bazel unexpectedly contains legacy traces extension wiring: $unwanted" >&2
        cat "$mod_a" >&2
        exit 1
    fi
done

# B-side: real install genrule replaces the placeholder.
b_build="$B/elements/hello-world/BUILD.bazel"
for marker in \
    'name = "hello-world_trace_build"' \
    'tags = ["trace_build"]' \
    '"install_tree.tar"' \
    '"trace.log"' \
    '"//tools:build-tracer"' \
    '"//tools:trace-publish"' \
    'cmake -B' \
    'cmake --build' \
    'cmake --install' \
    'CAS_GRPC_ADDR' \
    '--srckey='; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-cmake-round2-fallback: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done
if grep -qF 'BUILD_NOT_YET_STAGED' "$b_build"; then
    echo "meta-cmake-round2-fallback: B-side still has the placeholder; should have the install genrule" >&2
    cat "$b_build" >&2
    exit 1
fi

# srckey.txt is staged in B (trace-publish reads it).
if [ ! -f "$B/elements/hello-world/srckey.txt" ]; then
    echo "meta-cmake-round2-fallback: missing $B/elements/hello-world/srckey.txt" >&2
    exit 1
fi

# build-tracer + trace-publish + trace-lookup stage into both
# projects' tools/. Wiring all three at once means the
# trace-driven convergence research follow-on (teaching
# convert-element-cmake to consume @trace_<elem>//:trace to refine
# refusals into fine cc rules) is purely a converter-side
# change — no further write-a / staging work.
for path in \
    "$A/tools/build-tracer" \
    "$A/tools/trace-publish" \
    "$A/tools/trace-lookup" \
    "$B/tools/build-tracer" \
    "$B/tools/trace-publish" \
    "$B/tools/trace-lookup"; do
    if [ ! -x "$path" ]; then
        echo "meta-cmake-round2-fallback: missing executable $path" >&2
        exit 1
    fi
done

echo "meta-cmake-round2-fallback: render OK"
echo "meta-cmake-round2-fallback: ok"
