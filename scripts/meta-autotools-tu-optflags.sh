#!/bin/sh
# meta-autotools-tu-optflags.sh — acceptance gate for the
# per-target CFLAGS preservation path. The fixture has a hot
# translation unit with `hotloop.o: CFLAGS += -O2` overriding
# a global CFLAGS=-O0 -g. The trace captures the actual
# `cc -O0 -g -O2 -c hotloop.c` line; the make-db captures the
# per-target assignment. The converter cross-references both,
# strips the global -O0 / -g, and preserves the per-target
# -O2.
#
# Without per-target make-db awareness, all three flags would
# get stripped as default-toolchain — the cc_binary's copts
# would be empty and the per-target optimization intent would
# be lost.
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-autotools-native.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/build-tracer" ./cmd/build-tracer
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-autotools" ./cmd/convert-element-autotools

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

fixture="testdata/meta-project/autotools-tu-optflags"

"$bin_dir/write-a" \
    --bst "$fixture/optflags.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" \
    --convert-element-autotools "$bin_dir/convert-element-autotools" \
    --build-tracer-bin "$bin_dir/build-tracer"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-tu-optflags: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 7 ]; then
    echo "meta-autotools-tu-optflags: render OK; bazel < 7 (no bzlmod), skipping build phase"
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

run_bazel "$A" build //elements/optflags:optflags_install 2>&1 | tail -10

build_out="$A/bazel-bin/elements/optflags/BUILD.bazel.out"
if [ ! -f "$build_out" ]; then
    echo "meta-autotools-tu-optflags: BUILD.bazel.out not produced" >&2
    exit 1
fi

# The converter must preserve the per-target -O2 (intent flag).
# Without make-db awareness, -O2 would be stripped as default.
if ! grep -qF -- 'copts = ["-O2"]' "$build_out"; then
    echo "meta-autotools-tu-optflags: BUILD.bazel.out missing per-target -O2 in copts" >&2
    cat "$build_out" >&2
    exit 1
fi
# Negative: -O0 / -g (global default flags) shouldn't leak through.
for banned in '"-O0"' '"-g"'; do
    if grep -qF -- "$banned" "$build_out"; then
        echo "meta-autotools-tu-optflags: BUILD.bazel.out leaks default flag: $banned" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

echo "meta-autotools-tu-optflags: ok (per-target -O2 preserved; global -O0 -g stripped)"
echo "--- BUILD.bazel.out ---"
cat "$build_out"
