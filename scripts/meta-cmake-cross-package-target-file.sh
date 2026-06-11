#!/bin/sh
# meta-cmake-cross-package-target-file.sh — acceptance gate for
# PR 2 of cross-package `$<TARGET_FILE:t>` resolution.
#
# Drives the pipeline against testdata/meta-project/cross-package-
# target-file/, which has two kind:cmake elements: producer (a
# STATIC library that exports a cmake-config bundle) and consumer
# (a STATIC library whose CMakeLists.txt uses
# `find_package(producer CONFIG REQUIRED) +
# file(GENERATE OUTPUT tool_path.h CONTENT "...$<TARGET_FILE:producer::producer>...")`).
#
# The pipeline opts into the configure_file lift via
# --cmake-configure-file-bin (write-a then threads
# --lift-configure-file=true into each kind:cmake convert genrule and
# stages //tools:cmake-configure-file) — without the opt-in the
# converter lawfully takes the bake tier and no resolution happens,
# which is how this gate's build half was broken from birth.
#
# The gate asserts:
#   1. Render: write-a emits per-element BUILDs with an
#      imports.json on the consumer side mapping
#      producer::producer → //elements/producer:producer.
#   2. Bazel-build of the consumer's converter genrule:
#      cmake's find_package(producer CONFIG) resolves against
#      the staged bundle; the trace captures the file(GENERATE)
#      call; convert-element-cmake's file(GENERATE) lifter
#      sees the cross-package $<TARGET_FILE:producer::producer>
#      reference, looks it up in the imports manifest (expanding
#      the manifest's ${_IMPORT_PREFIX} link-path anchor against
#      the staged prefix so the (a)-evaluator's byte-equal check
#      matches), and emits a lifted cmake_configure_file whose
#      label-keyed target_files dict carries
#      `"//elements/producer:producer": "producer::producer"` —
#      Bazel resolves the artifact path at action time.
#   3. The refusal-stub tag
#      (`cmake-codegen-genex-cross-package`) does
#      NOT fire — the resolved-lift path replaces the PR 1
#      refusal stub for the manifest-hit case.
#   4. No convert-time absolute artifact path is baked into the
#      output (the (b)-capture failure shape: genex_values
#      embedding the recording machine's staged-prefix path,
#      which doesn't exist on the Bazel executor).
#
# Bazel-availability + cmake/ninja gating + META_BAZEL_*_ARGS
# overrides mirror scripts/meta-cross-cmake.sh; sandboxed
# environments without bcr.bazel.build egress can point bazel
# at github.com via a registry override.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake
CGO_ENABLED=0 go build -o "$bin_dir/cmake-configure-file" ./cmd/cmake-configure-file

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --cmake-configure-file-bin "$bin_dir/cmake-configure-file" \
    --bst testdata/meta-project/cross-package-target-file/producer.bst \
    --bst testdata/meta-project/cross-package-target-file/consumer.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks.
for want in \
    "elements/producer/BUILD.bazel" \
    "elements/consumer/BUILD.bazel" \
    "elements/consumer/imports.json"; do
    if [ ! -f "$A/$want" ]; then
        echo "meta-cmake-cross-package-target-file: missing rendered file in project A: $want" >&2
        exit 1
    fi
done
if ! grep -q '"bazel_label": "//elements/producer:producer"' "$A/elements/consumer/imports.json"; then
    echo "meta-cmake-cross-package-target-file: consumer imports.json missing producer mapping" >&2
    cat "$A/elements/consumer/imports.json" >&2
    exit 1
fi
echo "meta-cmake-cross-package-target-file: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-cmake-cross-package-target-file: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-cmake-cross-package-target-file: render OK; bazel $($BZL --version | head -1) is < 9, skipping build phase"
    exit 0
fi

# Bazel-build half execs convert-element-cmake which shells to
# cmake + ninja. Skip cleanly if either is missing.
for tool in cmake ninja; do
    if ! command -v "$tool" >/dev/null; then
        echo "meta-cmake-cross-package-target-file: render OK; $tool not on PATH, skipping build phase"
        exit 0
    fi
done

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

# Build both elements' converter genrules so the consumer can
# pick up producer's bundle and the trace.
run_bazel "$A" build //elements/producer:producer_converted //elements/consumer:consumer_converted 2>&1 | tail -10
consumer_build="$A/bazel-bin/elements/consumer/BUILD.bazel.out"
if [ ! -f "$consumer_build" ]; then
    echo "meta-cmake-cross-package-target-file: consumer BUILD.bazel.out not produced" >&2
    exit 1
fi

# === The PR 2 assertions ===============================================
#
# 1. The lifted file(GENERATE) cmake_configure_file's label-keyed
#    target_files dict maps the manifest-resolved cross-package
#    label, NOT the same-package shorthand — Bazel tracks the
#    producer as a dependency and resolves its artifact path at
#    action time.
want_entry='"//elements/producer:producer": "producer::producer"'
if ! grep -qF -- "$want_entry" "$consumer_build"; then
    echo "meta-cmake-cross-package-target-file: consumer BUILD.bazel.out missing expected target_files entry" >&2
    echo "want: $want_entry" >&2
    echo "--- consumer BUILD.bazel.out ---" >&2
    cat "$consumer_build" >&2
    exit 1
fi
echo "meta-cmake-cross-package-target-file: target_files resolved to cross-package label OK"

# 2. The (a) lift's tag IS present.
if ! grep -q '"cmake-codegen-genex-resolved"' "$consumer_build"; then
    echo "meta-cmake-cross-package-target-file: consumer BUILD.bazel.out missing (a) lift tag cmake-codegen-genex-resolved" >&2
    echo "--- consumer BUILD.bazel.out ---" >&2
    cat "$consumer_build" >&2
    exit 1
fi

# 3. The refusal-stub tag must NOT fire — the resolved-lift
#    path supersedes PR 1's refusal stub for manifest-hit cases.
if grep -q '"cmake-codegen-genex-cross-package"' "$consumer_build"; then
    echo "meta-cmake-cross-package-target-file: refusal-stub tag (cmake-codegen-genex-cross-package) fired but should NOT — manifest resolved producer::producer" >&2
    echo "--- consumer BUILD.bazel.out ---" >&2
    cat "$consumer_build" >&2
    exit 1
fi
echo "meta-cmake-cross-package-target-file: refusal-stub tag correctly absent"

# 4. No convert-time absolute artifact path baked into the output —
#    the (b)-capture failure shape embeds the recording machine's
#    staged-prefix path (e.g. /tmp/<stage>/lib/libproducer.a) in a
#    genex_values dict; that path doesn't exist on the executor.
if grep -qE '"/[^"]*lib/libproducer\.a"' "$consumer_build"; then
    echo "meta-cmake-cross-package-target-file: convert-time absolute producer artifact path baked into the output" >&2
    echo "--- consumer BUILD.bazel.out ---" >&2
    cat "$consumer_build" >&2
    exit 1
fi
echo "meta-cmake-cross-package-target-file: no convert-time absolute artifact path baked"

echo "meta-cmake-cross-package-target-file: ok"
