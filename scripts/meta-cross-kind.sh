#!/bin/sh
# meta-cross-kind.sh — render-half acceptance gate for the
# cross-element configure-step bootstrap (PR-4 cross-kind
# dependency case). Two-element fixture:
#
#   - auto-prod (kind:autotools): publishes a config bundle via
#     pass-3's trace_build genrule's synthesis step.
#   - cons (kind:cmake): consumes the bundle via :auto-prod_trace_load
#     at pass-2 (the action-time AC lookup).
#
# This gate asserts the rendered SHAPE — the wires are in place
# for the bundle to flow at build time. The live-AC end-to-end
# (bundle bytes actually round-tripping through a real REAPI
# endpoint and resolving cmake's find_package) is covered by
# tools/e2e-meta-cross-kind-live.sh, which needs docker +
# buildbarn + bb_clientd + bazel >= 9.

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

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/cross-kind/cons.bst \
    --bst testdata/meta-project/cross-kind/auto-prod.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup"

# Consumer's BUILD: the cmake handler emits the converter
# genrule consuming :auto-prod_trace_load (cross-element bundle
# source) + imports.json.
cons_build="$A/elements/cons/BUILD.bazel"
for marker in \
    '//elements/auto-prod:auto-prod_trace_load' \
    '"imports.json"' \
    '$$PREFIX' \
    'convert-element-cmake'; do
    if ! grep -qF -- "$marker" "$cons_build"; then
        echo "meta-cross-kind: consumer BUILD missing marker $marker" >&2
        cat "$cons_build" >&2
        exit 1
    fi
done
# Legacy kind=cmake filter would have produced no bundle ref for
# auto-prod (kind:autotools); confirm we aren't using the
# cmake-only bundle filegroup.
if grep -qF '//elements/auto-prod:cmake_config_bundle' "$cons_build"; then
    echo "meta-cross-kind: consumer BUILD unexpectedly references kind:cmake bundle for kind:autotools dep" >&2
    cat "$cons_build" >&2
    exit 1
fi

# Producer's BUILD: kind:autotools round-2 trace_load with
# expect_config_bundle=True, plus the trace_build genrule that
# synthesizes the bundle from $INSTALL_ROOT.
prod_a_build="$A/elements/auto-prod/BUILD.bazel"
for marker in \
    'name = "auto-prod_trace_load"' \
    'expect_config_bundle = True' \
    'load("@rules_buildstream_bazel//rules:traces.bzl", "trace_load")'; do
    if ! grep -qF -- "$marker" "$prod_a_build"; then
        echo "meta-cross-kind: producer A-side BUILD missing marker $marker" >&2
        cat "$prod_a_build" >&2
        exit 1
    fi
done

prod_b_build="$B/elements/auto-prod/BUILD.bazel"
for marker in \
    'name = "auto-prod_trace_build"' \
    'tags = ["trace_build"]' \
    'CONFIG_BUNDLE_DIR' \
    '--config-bundle=' \
    'lib/pkgconfig'; do
    if ! grep -qF -- "$marker" "$prod_b_build"; then
        echo "meta-cross-kind: producer B-side BUILD missing marker $marker" >&2
        cat "$prod_b_build" >&2
        exit 1
    fi
done

# imports.json: cons → auto-prod entry.
cons_imports="$A/elements/cons/imports.json"
if ! grep -qF '"auto-prod"' "$cons_imports"; then
    echo "meta-cross-kind: cons imports.json missing auto-prod entry" >&2
    cat "$cons_imports" >&2
    exit 1
fi

echo "meta-cross-kind: render OK"
echo "meta-cross-kind: ok"
