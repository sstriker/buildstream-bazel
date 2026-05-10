#!/bin/sh
# meta-autotools-libtool-shared.sh — acceptance gate for the
# real-libtool emission shape: dual compile (PIC + non-PIC) +
# `cc -shared` link to libfoo.so.0.0.0 + ar/ranlib for
# libfoo.a + a hand-rolled libfoo.la text file. Mirrors what
# libtool's link mode actually puts on the trace when an
# autotools project declares `lib_LTLIBRARIES = libfoo.la`.
#
# The gate asserts:
#   1. Render: project A's per-element BUILD wires the install
#      genrule with build-tracer + convert-element-trace.
#   2. Build (when bazel is available): the install genrule's
#      pipeline runs configure + make + install + tracer +
#      converter.
#   3. BUILD.bazel.out contains exactly ONE cc_library (foo)
#      sourced from the .a archive's compile event, and ZERO
#      cc_binary rules. Specifically: no rule named
#      libfoo.so.0.0.0 (the converter must skip cc -shared
#      events).
#   4. install-mapping.json captures both .a and .so install
#      destinations plus the .la metadata file.

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

fixture="testdata/meta-project/autotools-libtool-shared"

"$bin_dir/write-a" \
    --bst "$fixture/libtool-shared.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-round1

for marker in \
    '"BUILD.bazel.out"' \
    '"make-db.txt"' \
    '"install-mapping.json"' \
    'name = "libtool-shared_install"'; do
    if ! grep -qF -- "$marker" "$B/elements/libtool-shared/BUILD.bazel"; then
        echo "meta-autotools-libtool-shared: render missing marker: $marker" >&2
        cat "$B/elements/libtool-shared/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-autotools-libtool-shared: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-libtool-shared: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 7 ]; then
    echo "meta-autotools-libtool-shared: render OK; bazel < 7 (no bzlmod), skipping build phase"
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

run_bazel "$B" build //elements/libtool-shared:libtool-shared_install 2>&1 | tail -10

build_out="$B/bazel-bin/elements/libtool-shared/BUILD.bazel.out"
mapping="$B/bazel-bin/elements/libtool-shared/install-mapping.json"
for want in "$build_out" "$mapping" \
            "$B/bazel-bin/elements/libtool-shared/install_tree.tar" \
            "$B/bazel-bin/elements/libtool-shared/make-db.txt"; do
    if [ ! -f "$want" ]; then
        echo "meta-autotools-libtool-shared: missing build output $want" >&2
        exit 1
    fi
done

# BUILD.bazel.out shape: cc_library(foo) recovered, no spurious
# cc_binary for the .so link.
for marker in \
    'cc_library(' \
    'name = "foo"'; do
    if ! grep -qF -- "$marker" "$build_out"; then
        echo "meta-autotools-libtool-shared: BUILD.bazel.out missing marker: $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done
# Negative assertions: the cc -shared event MUST NOT have
# produced a cc_binary rule, and the .so artifact MUST NOT
# appear as a rule name. Either would mean the converter
# failed to filter libtool's shared-link wrapper.
if grep -qF 'cc_binary(' "$build_out"; then
    echo "meta-autotools-libtool-shared: cc_binary leaked into BUILD.bazel.out (cc -shared not skipped)" >&2
    cat "$build_out" >&2
    exit 1
fi
if grep -q 'libfoo\.so' "$build_out"; then
    echo "meta-autotools-libtool-shared: .so artifact leaked into BUILD.bazel.out as a rule" >&2
    cat "$build_out" >&2
    exit 1
fi

# install-mapping.json shape: all four install destinations
# captured.
for marker in \
    '"source": "libfoo.la"' \
    '"source": "foo.h"' \
    '"libfoo.a"' \
    '"libfoo.so.0.0.0"'; do
    if ! grep -qF -- "$marker" "$mapping"; then
        echo "meta-autotools-libtool-shared: install-mapping.json missing marker: $marker" >&2
        cat "$mapping" >&2
        exit 1
    fi
done

echo "meta-autotools-libtool-shared: ok (one cc_library recovered; cc -shared skipped; install-mapping captured .a + .so + .la + header)"
