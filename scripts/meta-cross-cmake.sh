#!/bin/sh
# meta-cross-cmake.sh — acceptance gate for cross-kind:cmake-element
# dependency staging.
#
# Drives the pipeline against testdata/meta-project/cross-cmake/, which
# has two kind:cmake elements: prod (a STATIC library that exports a
# cmake-config bundle) and cons (a STATIC library that does
# `find_package(prod CONFIG REQUIRED) + target_link_libraries(prod::prod)`).
#
# The gate asserts:
#   1. Render: write-a emits a per-element BUILD with cross-element
#      bundle staging in the cons genrule + an imports.json synthesis
#      file mapping prod::prod → //elements/prod:prod.
#   2. Bazel-build: bazel build //elements/cons:cons_converted in
#      project A. cmake's find_package(prod CONFIG) inside the
#      consumer action resolves against the staged bundle; the trace
#      records target_link_libraries(cons prod::prod); convert-element-cmake's
#      STATIC IMPORTED dep recovery (with the imports manifest) emits
#      `deps = ["//elements/prod:prod"]` in the consumer's BUILD.bazel.out.
#   3. Asserts the dep edge surfaces in the converted output.
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-hello.sh; sandboxed environments without bcr.bazel.build
# egress can point bazel at github.com via a registry override.

set -eu

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/cross-cmake/prod.bst \
    --bst testdata/meta-project/cross-cmake/cons.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks.
for want in \
    "elements/prod/BUILD.bazel" \
    "elements/cons/BUILD.bazel" \
    "elements/cons/imports.json"; do
    if [ ! -f "$A/$want" ]; then
        echo "meta-cross-cmake: missing rendered file in project A: $want" >&2
        exit 1
    fi
done
if ! grep -q '//elements/prod:cmake_config_bundle' "$A/elements/cons/BUILD.bazel"; then
    echo "meta-cross-cmake: cons genrule missing cross-element bundle ref" >&2
    exit 1
fi
if ! grep -q '"bazel_label": "//elements/prod:prod"' "$A/elements/cons/imports.json"; then
    echo "meta-cross-cmake: cons imports.json missing prod mapping" >&2
    exit 1
fi
echo "meta-cross-cmake: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-cross-cmake: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-cross-cmake: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"
run_bazel() {
    workspace="$1"
    cmd="$2"
    shift 2
    # shellcheck disable=SC2086
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

# Build both elements' converter genrules. cons needs prod's bundle
# staged on CMAKE_PREFIX_PATH (bazel pulls it transitively); the
# project-B phase below also needs prod's BUILD.bazel.out, so build
# prod's converter target explicitly too.
run_bazel "$A" build //elements/prod:prod_converted //elements/cons:cons_converted 2>&1 | tail -10
cons_build="$A/bazel-bin/elements/cons/BUILD.bazel.out"
if [ ! -f "$cons_build" ]; then
    echo "meta-cross-cmake: cons BUILD.bazel.out not produced" >&2
    exit 1
fi

# The dep edge is the whole point: the trace recovered prod::prod
# from target_link_libraries; the synthesized imports.json mapped
# it to //elements/prod; lower's STATIC IMPORTED dep recovery
# emitted it. The emitted label is the buildifier-normalized
# shorthand //elements/prod (≡ //elements/prod:prod).
if ! grep -q '"//elements/prod"' "$cons_build"; then
    echo "meta-cross-cmake: cons BUILD.bazel.out missing dep on //elements/prod" >&2
    head -30 "$cons_build" >&2
    exit 1
fi
echo "meta-cross-cmake: cross-element dep edge OK (cons → //elements/prod)"

# Producer-shipped cmake helper assertion: the prod fixture's
# install(FILES Helpers.cmake DESTINATION lib/cmake/prod) line
# must show up in the bundle tar so downstream consumers
# `include(${prod_DIR}/Helpers.cmake)` resolves.
prod_bundle="$A/bazel-bin/elements/prod/cmake-config-bundle.tar"
if ! tar -tf "$prod_bundle" | grep -q '^\./lib/cmake/prod/Helpers\.cmake$'; then
    echo "meta-cross-cmake: prod cmake-config-bundle.tar missing producer-shipped Helpers.cmake" >&2
    tar -tf "$prod_bundle" >&2
    exit 1
fi
echo "meta-cross-cmake: producer-shipped Helpers.cmake captured in bundle"

# === Project-B two-pass build ===========================================
# Stage project A's converted BUILD.bazel.out into project B and bazel-
# build the consumer there. This is the end-to-end check that the
# converted multi-element graph compiles as real Bazel cc rules with
# the cross-element dep wired and linking — the coverage re-homed from
# the orchestrator's e2e-bazel-build gate
# (docs/design/orchestrator-absorption.md). stage-b is the write-a +
# Bazel path's canonical A→B staging tool.
CGO_ENABLED=0 go build -o "$bin_dir/stage-b" ./cmd/stage-b
"$bin_dir/stage-b" --project-a "$A" --project-b "$B" >/dev/null
for want in elements/prod/BUILD.bazel elements/cons/BUILD.bazel; do
    if grep -q BUILD_NOT_YET_STAGED "$B/$want"; then
        echo "meta-cross-cmake: stage-b left the placeholder in project B's $want" >&2
        exit 1
    fi
done
# cons is a cc_library that deps //elements/prod:prod; building it in
# project B compiles cons's TUs and resolves + links the cross-element
# converted dep.
run_bazel "$B" build //elements/cons:cons 2>&1 | tail -10
echo "meta-cross-cmake: project B built //elements/cons:cons — cross-element converted cc deps link end-to-end"
