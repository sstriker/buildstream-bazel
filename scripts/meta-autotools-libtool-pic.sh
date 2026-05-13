#!/bin/sh
# meta-autotools-libtool-pic.sh — acceptance gate for the
# libtool-style dual-compile pattern.
#
# In real libtool projects the same translation unit gets
# compiled twice: once with -fPIC -DPIC into `.libs/foo.o`
# (archived as the shared-library precursor) and once
# without PIC into `foo.o` (archived as the static lib).
# Both produce object files with the same basename.
#
# The fixture's Makefile.in:
#
#   cc -O2 -c foo.c -o foo.o            # non-PIC
#   ar rcs libfoo.a foo.o
#   cc -O2 -fPIC -DPIC -c foo.c -o .libs/foo.o
#   ar rcs libfoo_pic.a .libs/foo.o
#
# Without exact-path correlation in the converter, both
# archives' lookups by basename `foo.o` would resolve to
# the same compile event (last write wins) — so the static
# lib would inherit -DPIC from the PIC compile, producing
# a wrong build (cc_library(foo) with defines=["PIC"]).
#
# This gate asserts:
#   - libfoo.a → cc_library(name="foo") WITHOUT defines=["PIC"]
#   - libfoo_pic.a → cc_library(name="foo_pic") WITH defines=["PIC"]
#   - -fPIC stripped from both (default-toolchain, Bazel handles it)
#   - install-mapping.json captures both archives + foo.h header
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides
# mirror scripts/meta-autotools-native.sh.

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

fixture="testdata/meta-project/autotools-libtool-pic"

"$bin_dir/write-a" \
    --bst "$fixture/libtool-pic.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-round1

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-libtool-pic: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-autotools-libtool-pic: render OK; bazel < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

run_bazel "$B" build //elements/libtool-pic:libtool-pic_install 2>&1 | tail -10

build_out="$B/bazel-bin/elements/libtool-pic/BUILD.bazel.out"
mapping="$B/bazel-bin/elements/libtool-pic/install-mapping.json"
for want in "$build_out" "$mapping"; do
    if [ ! -f "$want" ]; then
        echo "meta-autotools-libtool-pic: missing build output $want" >&2
        exit 1
    fi
done

# Both rules must exist.
for marker in \
    'name = "foo"' \
    'name = "foo_pic"' \
    'srcs = ["foo.c"]'; do
    if ! grep -qF -- "$marker" "$build_out"; then
        echo "meta-autotools-libtool-pic: BUILD.bazel.out missing marker: $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

# foo_pic carries the -DPIC define from libtool's PIC compile.
# Use awk to scope the check to the foo_pic rule body — a bare
# grep would match across both rules.
if ! awk '/^cc_library\($/,/^\)$/{print}' "$build_out" | \
   awk 'BEGIN{RS="\n)\n"} /name = "foo_pic"/' | \
   grep -qF 'defines = ["PIC"]'; then
    echo "meta-autotools-libtool-pic: foo_pic missing defines=[\"PIC\"]" >&2
    cat "$build_out" >&2
    exit 1
fi

# foo (the non-PIC static lib) must NOT carry the PIC define —
# its compile event came from `cc -O2 -c foo.c -o foo.o`, no -DPIC.
# This is the libtool collision-avoidance assertion: without
# exact-path correlation the lookup-by-basename "foo.o" would
# resolve both archives to the same (last-written) PIC compile.
if awk '/^cc_library\($/,/^\)$/{print}' "$build_out" | \
   awk 'BEGIN{RS="\n)\n"} /name = "foo"$|name = "foo",/' | \
   grep -qF 'defines = ["PIC"]'; then
    echo "meta-autotools-libtool-pic: static foo lib leaks PIC define from libtool dual-compile collision" >&2
    cat "$build_out" >&2
    exit 1
fi

# -fPIC must NOT leak through into either rule's copts (it's a
# default-toolchain flag — rules_cc handles PIC under linkshared,
# fPIC in copts would be redundant and confusing).
for banned in '"-fPIC"' '"-fpic"'; do
    if grep -qF -- "$banned" "$build_out"; then
        echo "meta-autotools-libtool-pic: BUILD.bazel.out leaks PIC flag: $banned" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

# install-mapping.json captures both archives + the header.
for marker in \
    '"source": "libfoo.a"' \
    '"source": "libfoo_pic.a"' \
    '"source": "foo.h"' \
    '"rule": "foo"' \
    '"rule": "foo_pic"'; do
    if ! grep -qF -- "$marker" "$mapping"; then
        echo "meta-autotools-libtool-pic: install-mapping.json missing marker: $marker" >&2
        cat "$mapping" >&2
        exit 1
    fi
done

echo "meta-autotools-libtool-pic: ok (libtool dual-compile resolved by exact path; -DPIC isolated to foo_pic)"
echo "--- BUILD.bazel.out ---"
cat "$build_out"
