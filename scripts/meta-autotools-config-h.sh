#!/bin/sh
# meta-autotools-config-h.sh — acceptance gate for build-time-
# generated header tracking (AC_CONFIG_HEADERS shape). The
# fixture's configure step produces config.h from config.h.in;
# foo.c does `#include "config.h"`. Without --generated-headers
# threading, BUILD.bazel.out's cc_library would have no record
# of config.h and round-2 cc compile would fail to resolve the
# header.
#
# The gate asserts:
#   1. Render: project A's per-element BUILD wires the install
#      genrule with a generated-headers.txt output and a
#      --generated-headers= flag on the converter call.
#   2. Build (when bazel is available): the install genrule's
#      pipeline snapshots pre/post-configure header sets and
#      diffs them; generated-headers.txt lists config.h.
#   3. BUILD.bazel.out's cc_library carries `hdrs = ["config.h"]`.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/build-tracer" ./cmd/build-tracer
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-trace" ./cmd/convert-element-trace

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

fixture="testdata/meta-project/autotools-config-h"

"$bin_dir/write-a" \
    --bst "$fixture/config-h.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-round1

for marker in \
    '"BUILD.bazel.out"' \
    '"make-db.txt"' \
    '"install-mapping.json"' \
    '"generated-headers.txt"' \
    '--generated-headers="$(location generated-headers.txt)"' \
    'PRE_HEADERS_LIST="$$(mktemp)"' \
    'comm -13 "$$PRE_HEADERS_LIST" "$$POST_HEADERS_LIST"' \
    'name = "config-h_install"'; do
    if ! grep -qF -- "$marker" "$B/elements/config-h/BUILD.bazel"; then
        echo "meta-autotools-config-h: render missing marker: $marker" >&2
        cat "$B/elements/config-h/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-autotools-config-h: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-config-h: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-autotools-config-h: render OK; bazel < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

run_bazel "$B" build //elements/config-h:config-h_install 2>&1 | tail -10

build_out="$B/bazel-bin/elements/config-h/BUILD.bazel.out"
generated_headers="$B/bazel-bin/elements/config-h/generated-headers.txt"
for want in "$build_out" "$generated_headers" \
            "$B/bazel-bin/elements/config-h/install_tree.tar"; do
    if [ ! -f "$want" ]; then
        echo "meta-autotools-config-h: missing build output $want" >&2
        exit 1
    fi
done

# generated-headers.txt should list config.h (post-configure
# diff against the source-only pre-configure snapshot).
if ! grep -q 'config\.h' "$generated_headers"; then
    echo "meta-autotools-config-h: generated-headers.txt missing config.h" >&2
    cat "$generated_headers" >&2
    exit 1
fi

# BUILD.bazel.out's cc_library carries hdrs.
for marker in \
    'cc_library(' \
    'name = "foo"' \
    'hdrs = ["config.h"]'; do
    if ! grep -qF -- "$marker" "$build_out"; then
        echo "meta-autotools-config-h: BUILD.bazel.out missing marker: $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

echo "meta-autotools-config-h: ok (config.h detected as build-time-generated header; cc_library carries it in hdrs)"
