#!/bin/sh
# meta-cmake-round2-fallback-multiplatform.sh — render-half
# acceptance gate for kind:cmake's Phase B execute_process
# round-2 fallback under --platforms-json (the multi-platform
# install fan-out, sibling of meta-trace-round2-fold and
# meta-autotools-round2-multiplatform).
#
# When the operator passes --cmake-round2-fallback +
# --platforms-json to write-a, project B's per-element render
# fans out to N install genrules (one per platform) + a
# top-level select()-filegroup at :install_tree.tar. Each
# install genrule:
#
#   - Names "<elem>_install_<platform>"
#   - Outputs land under <platform>/install_tree.tar +
#     <platform>/trace.log
#   - exec_compatible_with carries the constraint set
#   - trace-publish bakes --platform=<plat> literally so each
#     cell publishes under its own AC partition
#
# Project A's converter genrule is unchanged here — under
# multi-platform mode, the orchestrator's existing kind:cmake
# multi-platform fan-out (PR #112) runs convert-element-cmake N
# times per element AT ORCHESTRATE TIME, then fold-element
# composes the per-platform IRs. Write-a's project A output
# for cmake round-2-fallback is the same shape the existing
# meta-cmake-round2-fallback gate verifies.
#
# Bazel-build half out of scope; live-AC contract covered by
# the kind-agnostic tools/e2e-meta-autotools-round2-live.sh.

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

fixture="testdata/meta-project"

# --platforms-json requires the trace-driven round-2 path; we
# supply --convert-element-trace + --trace-round1 is NOT set so
# round-2 is on. --cmake-round2-fallback turns on cmake's
# fallback shape. Both together exercise the cmake round-2
# fallback + multi-platform install fan-out.
"$bin_dir/write-a" \
    --bst "$fixture/hello-world.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup" \
    --fold-element-bin "$bin_dir/fold-element" \
    --cmake-round2-fallback \
    --platforms-json "$platforms_json"

# Project B: N install genrules + top-level filegroup.
b_build="$B/elements/hello-world/BUILD.bazel"
for marker in \
    'name = "hello-world_install_linux_x86_64"' \
    'name = "hello-world_install_darwin_arm64"' \
    '"linux_x86_64/install_tree.tar"' \
    '"darwin_arm64/install_tree.tar"' \
    '"linux_x86_64/trace.log"' \
    '"darwin_arm64/trace.log"' \
    'exec_compatible_with = [' \
    '"@platforms//cpu:x86_64",' \
    '"@platforms//os:linux",' \
    '"@platforms//cpu:arm64",' \
    '"@platforms//os:darwin",' \
    '--platform="linux_x86_64"' \
    '--platform="darwin_arm64"' \
    'name = "install_tree.tar"' \
    '"@platforms//cpu:x86_64": ["linux_x86_64/install_tree.tar"]' \
    '"@platforms//cpu:arm64": ["darwin_arm64/install_tree.tar"]' \
    '"//conditions:default": [],' \
    'cmake -B' \
    'cmake --build' \
    'cmake --install'; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-cmake-round2-fallback-multiplatform: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done

# Legacy single-platform genrule name must NOT appear under
# multi-platform mode.
if grep -qF -- 'name = "hello-world_install"' "$b_build"; then
    echo "meta-cmake-round2-fallback-multiplatform: B-side unexpectedly contains legacy 'hello-world_install' name (no platform suffix)" >&2
    cat "$b_build" >&2
    exit 1
fi

# Project A's //platforms package: one platform() per declared
# --platforms-json entry with constraint_values + the
# reapi_properties-derived exec_properties dict.
platforms_build="$A/platforms/BUILD.bazel"
for marker in \
    'name = "linux_x86_64",' \
    'name = "darwin_arm64",' \
    'constraint_values = [' \
    'exec_properties = {' \
    '"container-image": "docker://debian:bookworm",'; do
    if ! grep -qF -- "$marker" "$platforms_build"; then
        echo "meta-cmake-round2-fallback-multiplatform: platforms/BUILD.bazel missing marker: $marker" >&2
        cat "$platforms_build" 2>&1 >&2
        exit 1
    fi
done

echo "meta-cmake-round2-fallback-multiplatform: render OK"
echo "meta-cmake-round2-fallback-multiplatform: ok"
