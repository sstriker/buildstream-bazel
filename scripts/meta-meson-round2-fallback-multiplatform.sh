#!/bin/sh
# meta-meson-round2-fallback-multiplatform.sh — render-half
# acceptance gate for kind:meson's Phase B round-2 fallback under
# --platforms-json (the multi-platform install fan-out, sibling
# of meta-cmake-round2-fallback-multiplatform.sh and
# meta-autotools-round2-multiplatform.sh).
#
# When the operator passes --meson-round2-fallback +
# --platforms-json to write-a, project B's per-element render
# fans out to N install genrules (one per platform) + a
# top-level select()-filegroup at :install_tree.tar. Each
# install genrule:
#
#   - Names "<elem>_trace_build_<platform>"
#   - Outputs land under <platform>/install_tree.tar +
#     <platform>/trace.log
#   - exec_compatible_with carries the constraint set
#   - trace-publish bakes --platform=<plat> literally so each
#     cell publishes under its own AC partition
#
# Project A's per-platform converter fan-out (renderTraceDrivenRound2A,
# kind-agnostic since PR #112) was already covered by the
# autotools / cmake siblings; this gate's primary signal is the
# B-side install fan-out — the Phase B sibling that the ROADMAP
# tagged for promotion once a fixture surfaced the need.
#
# Bazel-build half intentionally out of scope; the kind-agnostic
# live-AC contract is exercised by tools/e2e-meta-autotools-round2-
# live.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-meson" ./converter/cmd/convert-element-meson
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

fixture="testdata/meta-project"

# --platforms-json requires the trace-driven round-2 plumbing
# (convert-element-trace + the trace-publish/lookup pair +
# fold-element). --meson-round2-fallback turns on meson's
# install-plan-driven fallback shape. Together they exercise the
# meson Phase B install fan-out.
"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/meson-greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-meson "$bin_dir/convert-element-meson" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup" \
    --fold-element-bin "$bin_dir/fold-element" \
    --meson-round2-fallback \
    --platforms-json "$platforms_json"

# Project B: N install genrules + top-level filegroup. Each
# per-platform genrule's outputs land under <platform>/ so the
# two cells don't collide; exec_compatible_with carries each
# cell's constraint set; trace-publish bakes --platform=<plat>
# literally so each cell publishes under its own AC partition.
b_build="$B/elements/meson-greet/BUILD.bazel"
for marker in \
    'name = "meson-greet_trace_build_linux_x86_64"' \
    'name = "meson-greet_trace_build_darwin_arm64"' \
    '"linux_x86_64/trace.log"' \
    '"darwin_arm64/trace.log"' \
    'exec_compatible_with = [' \
    '"@platforms//cpu:x86_64",' \
    '"@platforms//os:linux",' \
    '"@platforms//cpu:arm64",' \
    '"@platforms//os:darwin",' \
    '--platform="linux_x86_64"' \
    '--platform="darwin_arm64"' \
    'name = "meson-greet_install"' \
    '":meson-greet_trace_build_linux_x86_64"' \
    '":meson-greet_trace_build_darwin_arm64"' \
    '"//conditions:default": [],' \
    'meson setup' \
    '--prefix=/' \
    '--libdir=lib' \
    'ninja -C' \
    'meson install'; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-meson-round2-fallback-multiplatform: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done

# Legacy single-platform genrule name must NOT appear under
# multi-platform mode.
if grep -qF -- 'name = "meson-greet_trace_build"' "$b_build"; then
    echo "meta-meson-round2-fallback-multiplatform: B-side unexpectedly contains legacy 'meson-greet_trace_build' name (no platform suffix)" >&2
    cat "$b_build" >&2
    exit 1
fi

# Project A's //platforms package: one platform() per declared
# --platforms-json entry with constraint_values + the
# reapi_properties-derived exec_properties dict. Mirrors the
# assertion in meta-{cmake,autotools}-round2-fallback-multiplatform
# — the same write-a code path emits //platforms regardless of
# which round-2 kind triggers --platforms-json.
platforms_build="$A/platforms/BUILD.bazel"
for marker in \
    'name = "linux_x86_64",' \
    'name = "darwin_arm64",' \
    'constraint_values = [' \
    'exec_properties = {' \
    '"container-image": "docker://debian:bookworm",'; do
    if ! grep -qF -- "$marker" "$platforms_build"; then
        echo "meta-meson-round2-fallback-multiplatform: platforms/BUILD.bazel missing marker: $marker" >&2
        cat "$platforms_build" 2>&1 >&2
        exit 1
    fi
done

echo "meta-meson-round2-fallback-multiplatform: render OK"
echo "meta-meson-round2-fallback-multiplatform: ok"
