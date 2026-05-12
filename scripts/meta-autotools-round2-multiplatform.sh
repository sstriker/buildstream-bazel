#!/bin/sh
# meta-autotools-round2-multiplatform.sh — render-half acceptance
# gate for kind:autotools' per-platform install fan-out under
# round-2.
#
# Sibling of meta-trace-round2-fold.sh (which exercises the same
# fan-out for pipelineHandler kinds — kind:make / manual / script
# / makemaker / modulebuild). Same rendered shape, different
# handler dispatch site (autotoolsHandler in
# handler_autotools_native.go vs pipelineHandler in
# handler_pipeline.go). Verifies:
#
#   1. Project A emits N (=2) per-platform converter genrules
#      named "<elem>__<platform>_ir" + a fold-element genrule
#      keeping the legacy "<elem>_build" name (from PR #112's
#      project A fan-out, kind-agnostic via renderTraceDrivenRound2A).
#   2. Project B emits N install genrules named
#      "<elem>_install_<platform>" + a top-level
#      install_tree.tar select()-filegroup. Each install
#      genrule's outputs land under <platform>/, exec_compatible_with
#      carries the constraint set, and trace-publish bakes
#      --platform=<plat> literally so each cell publishes under
#      its own AC partition.
#   3. tools/traces.json has one entry per (element, platform)
#      cell.
#
# Bazel-build half is intentionally out of scope; the live-AC
# wire contract is exercised by tools/e2e-meta-autotools-round2-
# live.sh which is kind-agnostic.

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

fixture="testdata/meta-project/autotools-greet"

"$bin_dir/write-a" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup" \
    --fold-element-bin "$bin_dir/fold-element" \
    --platforms-json "$platforms_json"

# Project A: two per-platform converter genrules + fold genrule
# (kind-agnostic, shipped in PR #112).
a_build="$A/elements/greet/BUILD.bazel"
for marker in \
    'name = "greet__linux_x86_64_ir"' \
    'name = "greet__darwin_arm64_ir"' \
    'name = "greet_build"' \
    '"@trace_greet__linux_x86_64//:trace"' \
    '"@trace_greet__darwin_arm64//:trace"' \
    '"//tools:fold-element"'; do
    if ! grep -qF -- "$marker" "$a_build"; then
        echo "meta-autotools-round2-multiplatform: A-side BUILD missing marker: $marker" >&2
        cat "$a_build" >&2
        exit 1
    fi
done

# Project B: N install genrules + top-level filegroup.
b_build="$B/elements/greet/BUILD.bazel"
for marker in \
    'name = "greet_install_linux_x86_64"' \
    'name = "greet_install_darwin_arm64"' \
    '"linux_x86_64/install_tree.tar"' \
    '"darwin_arm64/install_tree.tar"' \
    '"linux_x86_64/trace.log"' \
    '"darwin_arm64/trace.log"' \
    '"linux_x86_64/generated-headers.txt"' \
    '"darwin_arm64/generated-headers.txt"' \
    'exec_compatible_with = ["@platforms//cpu:x86_64", "@platforms//os:linux"]' \
    'exec_compatible_with = ["@platforms//cpu:arm64", "@platforms//os:darwin"]' \
    '--platform="linux_x86_64"' \
    '--platform="darwin_arm64"' \
    '$(location linux_x86_64/generated-headers.txt)' \
    '$(location darwin_arm64/generated-headers.txt)' \
    'name = "install_tree.tar"' \
    '"@platforms//cpu:x86_64": ["linux_x86_64/install_tree.tar"]' \
    '"@platforms//cpu:arm64": ["darwin_arm64/install_tree.tar"]' \
    '"//conditions:default": [],'; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-autotools-round2-multiplatform: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done

# Legacy single-platform genrule name must NOT appear under
# multi-platform mode.
if grep -qF -- 'name = "greet_install"' "$b_build"; then
    echo "meta-autotools-round2-multiplatform: B-side unexpectedly contains legacy 'greet_install' name (no platform suffix)" >&2
    cat "$b_build" >&2
    exit 1
fi

# tools/traces.json: per-(element, platform) entries.
traces_json="$A/tools/traces.json"
for marker in \
    '"key": "greet__linux_x86_64"' \
    '"key": "greet__darwin_arm64"' \
    '"platform": "linux_x86_64"' \
    '"platform": "darwin_arm64"'; do
    if ! grep -qF -- "$marker" "$traces_json"; then
        echo "meta-autotools-round2-multiplatform: traces.json missing marker: $marker" >&2
        cat "$traces_json" >&2
        exit 1
    fi
done

echo "meta-autotools-round2-multiplatform: render OK"
echo "meta-autotools-round2-multiplatform: ok"
