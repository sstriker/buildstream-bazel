#!/bin/sh
# meta-trace-round2-fold.sh — render-half acceptance gate for the
# per-platform fold of round-2 trace-driven kinds.
#
# Exercises the same kind:make fixture as meta-make-round2.sh, but
# with a two-platform manifest passed via --platforms-json. Verifies
# that:
#
#   1. Project A emits N (=2) per-platform converter genrules instead
#      of one — names like "<elem>__linux_x86_64_ir" — each consuming
#      its platform-tagged trace repo (@trace_<elem>__<platform>//:trace)
#      and emitting <platform>/ir.json + <platform>/BUILD.bazel.out.
#   2. A trailing fold-element genrule keeps the legacy "<elem>_build"
#      name (so downstream wiring stays valid) and composes the N
#      ir.json files via fold-element + elementfold + emit/bazel.
#   3. The --cell argv for fold-element carries each platform's
#      constraints + ir.json $(location) reference.
#   4. tools/traces.json contains one entry per (element, platform)
#      cell keyed "<elem>__<platform>" with the platform field set.
#   5. The legacy single @trace_<elem>//:trace label is absent in
#      multi-platform mode — every trace reference is platform-tagged.
#
# Bazel-build half is intentionally out of scope: this is render-shape
# verification only. The live-AC contract (per-platform trace publish
# + lookup rendezvous) is already covered by the kind-agnostic
# tools/e2e-meta-autotools-round2-live.sh.
#
# Scope: pipelineHandler-shaped trace-driven kinds (kind:make /
# manual / script / makemaker / modulebuild) today. kind:autotools'
# autotoolsHandler dispatch + kind:cmake Phase B fallback + kind:meson
# Phase B render are queued follow-ups; their per-platform render
# shape will mirror this gate's expectations once wired.

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
CGO_ENABLED=0 go build -o "$bin_dir/fold-element" ./converter/cmd/fold-element

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"
platforms_json="$work_dir/platforms.json"

cat >"$platforms_json" <<'JSON'
[
  {
    "name": "linux_x86_64",
    "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"],
    "reapi_properties": [{"name": "container-image", "value": "docker://debian:bookworm"}]
  },
  {
    "name": "darwin_arm64",
    "constraints": ["@platforms//os:darwin", "@platforms//cpu:arm64"],
    "reapi_properties": [{"name": "container-image", "value": "docker://debian:bookworm"}]
  }
]
JSON

fixture="testdata/meta-project/make-greet"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup" \
    --fold-element-bin "$bin_dir/fold-element" \
    --platforms-json "$platforms_json"

a_build="$A/elements/greet/BUILD.bazel"

# Two per-platform converter genrules + one fold genrule retaining
# the legacy "<elem>_build" name. Each per-platform converter reads
# its platform-tagged :greet_trace_load_<platform> target and
# emits ir.json under <platform>/.
for marker in \
    'name = "greet__linux_x86_64_ir"' \
    'name = "greet__darwin_arm64_ir"' \
    'name = "greet_build"' \
    'load("@rules_buildstream_bazel//rules:traces.bzl", "trace_load")' \
    'name = "greet_trace_load_linux_x86_64"' \
    'name = "greet_trace_load_darwin_arm64"' \
    'platform = "linux_x86_64"' \
    'platform = "darwin_arm64"' \
    '":greet_trace_load_linux_x86_64"' \
    '":greet_trace_load_darwin_arm64"' \
    '"linux_x86_64/ir.json"' \
    '"darwin_arm64/ir.json"' \
    '"linux_x86_64/BUILD.bazel.out"' \
    '"darwin_arm64/BUILD.bazel.out"' \
    '--out-ir-json' \
    '"//tools:fold-element"' \
    "--cell 'linux_x86_64|@platforms//os:linux,@platforms//cpu:x86_64|\$(location linux_x86_64/ir.json)'" \
    "--cell 'darwin_arm64|@platforms//os:darwin,@platforms//cpu:arm64|\$(location darwin_arm64/ir.json)'"; do
    if ! grep -qF -- "$marker" "$a_build"; then
        echo "meta-trace-round2-fold: A-side BUILD missing marker: $marker" >&2
        cat "$a_build" >&2
        exit 1
    fi
done

# Legacy load-time external-repo trace labels must NOT appear —
# every trace reference is a same-package trace_load target.
for banned in \
    '"@trace_greet//:trace"' \
    '"@trace_greet__linux_x86_64//:trace"' \
    '"@trace_greet__darwin_arm64//:trace"'; do
    if grep -qF -- "$banned" "$a_build"; then
        echo "meta-trace-round2-fold: A-side unexpectedly contains legacy external-repo label $banned" >&2
        cat "$a_build" >&2
        exit 1
    fi
done

# tools/traces.json is no longer emitted — the trace_load rule
# carries srckey + platform as string attrs directly.
if [ -f "$A/tools/traces.json" ]; then
    echo "meta-trace-round2-fold: $A/tools/traces.json unexpectedly emitted (legacy load-time wiring)" >&2
    exit 1
fi

# MODULE.bazel must NOT declare the legacy `traces` extension.
mod_a="$A/MODULE.bazel"
for unwanted in \
    'use_extension("//rules:traces.bzl", "traces")' \
    '"trace_greet__linux_x86_64"' \
    '"trace_greet__darwin_arm64"'; do
    if grep -qF -- "$unwanted" "$mod_a"; then
        echo "meta-trace-round2-fold: A MODULE.bazel unexpectedly contains legacy traces extension wiring: $unwanted" >&2
        cat "$mod_a" >&2
        exit 1
    fi
done

# fold-element binary staged into project A's tools/.
if [ ! -x "$A/tools/fold-element" ]; then
    echo "meta-trace-round2-fold: missing executable $A/tools/fold-element" >&2
    exit 1
fi

# Project B side: N install genrules + top-level :install_tree.tar
# filegroup that select()s per-platform. Each genrule's outputs land
# under <platform>/, exec_compatible_with carries the constraint
# set, and trace-publish bakes --platform=<plat> literally so each
# cell publishes under its own AC partition.
b_build="$B/elements/greet/BUILD.bazel"
for marker in \
    'name = "greet_trace_build_linux_x86_64"' \
    'name = "greet_trace_build_darwin_arm64"' \
    '"linux_x86_64/install_tree.tar"' \
    '"darwin_arm64/install_tree.tar"' \
    '"linux_x86_64/trace.log"' \
    '"darwin_arm64/trace.log"' \
    '"linux_x86_64/make-db.txt"' \
    '"darwin_arm64/make-db.txt"' \
    'exec_compatible_with = [' \
    '"@platforms//cpu:x86_64",' \
    '"@platforms//os:linux",' \
    '"@platforms//cpu:arm64",' \
    '"@platforms//os:darwin",' \
    '--platform="linux_x86_64"' \
    '--platform="darwin_arm64"' \
    '$(location linux_x86_64/generated-headers.txt)' \
    '$(location darwin_arm64/generated-headers.txt)' \
    'name = "install_tree.tar"' \
    '"@platforms//cpu:x86_64": ["linux_x86_64/install_tree.tar"]' \
    '"@platforms//cpu:arm64": ["darwin_arm64/install_tree.tar"]' \
    '"//conditions:default": [],'; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-trace-round2-fold: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done

# Legacy single-platform genrule name must NOT appear under
# multi-platform mode.
if grep -qF -- 'name = "greet_trace_build"' "$b_build"; then
    echo "meta-trace-round2-fold: B-side unexpectedly contains legacy 'greet_install' name (no platform suffix)" >&2
    cat "$b_build" >&2
    exit 1
fi

# Project A's //platforms package: one platform() per declared
# --platforms-json entry, carrying constraint_values + the
# reapi_properties-derived exec_properties dict. The per-element
# converter genrules already carry exec_compatible_with =
# <constraints>; registering these as --extra_execution_platforms
# routes each genrule to the matching Buildbarn worker pool.
platforms_build="$A/platforms/BUILD.bazel"
if [ ! -f "$platforms_build" ]; then
    echo "meta-trace-round2-fold: missing $platforms_build" >&2
    exit 1
fi
for marker in \
    'platform(' \
    'name = "linux_x86_64",' \
    'name = "darwin_arm64",' \
    'constraint_values = [' \
    '"@platforms//os:linux",' \
    '"@platforms//cpu:arm64",' \
    'exec_properties = {' \
    '"container-image": "docker://debian:bookworm",' \
    'visibility = ["//visibility:public"],'; do
    if ! grep -qF -- "$marker" "$platforms_build"; then
        echo "meta-trace-round2-fold: platforms/BUILD.bazel missing marker: $marker" >&2
        cat "$platforms_build" >&2
        exit 1
    fi
done

echo "meta-trace-round2-fold: render OK"
echo "meta-trace-round2-fold: ok"
