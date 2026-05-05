#!/bin/sh
# meta-autotools-multitarget.sh — acceptance gate for the
# trace-driven kind:autotools native render path against a
# fixture exercising:
#   - multiple cc_library archives (libappcore.a, libmathlib.a)
#   - multiple cc_binary outputs with different install dests
#     (bin/app, libexec/helper)
#   - per-target CFLAGS overrides (helper.o gets -Wall)
#   - install layout with bin/, libexec/, lib/, include/,
#     share/multitarget/
#
# The gate asserts:
#   1. Render: project A's per-element BUILD wires the install
#      genrule with build-tracer + convert-element-autotools +
#      install-mapping.json output.
#   2. Build: bazel build runs the tracer-wrapped pipeline +
#      converter, producing install_tree.tar +
#      BUILD.bazel.out + make-db.txt + install-mapping.json
#      in one action.
#   3. BUILD.bazel.out contains the four expected rules
#      (cc_library appcore + mathlib, cc_binary app + helper)
#      with the right deps wiring (app links both libs;
#      helper links appcore) and per-target copts (helper has
#      -Wall).
#   4. install-mapping.json captures all seven install
#      destinations with rule cross-references on the
#      buildable artifacts.
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

fixture="testdata/meta-project/autotools-multitarget"

"$bin_dir/write-a" \
    --bst "$fixture/multitarget.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" \
    --convert-element-autotools "$bin_dir/convert-element-autotools" \
    --build-tracer-bin "$bin_dir/build-tracer"

for marker in \
    '"BUILD.bazel.out"' \
    '"make-db.txt"' \
    '"install-mapping.json"' \
    'name = "multitarget_install"' \
    '--out-install-mapping="$(location install-mapping.json)"'; do
    if ! grep -qF -- "$marker" "$B/elements/multitarget/BUILD.bazel"; then
        echo "meta-autotools-multitarget: render missing marker: $marker" >&2
        cat "$B/elements/multitarget/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-autotools-multitarget: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-multitarget: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 7 ]; then
    echo "meta-autotools-multitarget: render OK; bazel < 7 (no bzlmod), skipping build phase"
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

run_bazel "$B" build //elements/multitarget:multitarget_install 2>&1 | tail -10

build_out="$B/bazel-bin/elements/multitarget/BUILD.bazel.out"
mapping="$B/bazel-bin/elements/multitarget/install-mapping.json"
for want in "$build_out" "$mapping" \
            "$B/bazel-bin/elements/multitarget/install_tree.tar" \
            "$B/bazel-bin/elements/multitarget/make-db.txt"; do
    if [ ! -f "$want" ]; then
        echo "meta-autotools-multitarget: missing build output $want" >&2
        exit 1
    fi
done

# BUILD.bazel.out shape.
for marker in \
    'cc_library(' 'name = "appcore"' \
    'cc_library(' 'name = "mathlib"' \
    'cc_binary(' 'name = "app"' \
    'cc_binary(' 'name = "helper"' \
    'deps = [":appcore", ":mathlib"]' \
    'deps = [":appcore"]' \
    'copts = ["-Wall"]'; do
    if ! grep -qF -- "$marker" "$build_out"; then
        echo "meta-autotools-multitarget: BUILD.bazel.out missing marker: $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

# install-mapping.json shape.
for marker in \
    '"source": "app"' \
    '"source": "helper"' \
    '"source": "libmathlib.a"' \
    '"source": "libappcore.a"' \
    '"source": "include/mathlib.h"' \
    '"source": "include/appcore.h"' \
    '"source": "share/multitarget-readme.txt"' \
    '"rule": "app"' \
    '"rule": "helper"' \
    '"rule": "mathlib"' \
    '"rule": "appcore"'; do
    if ! grep -qF -- "$marker" "$mapping"; then
        echo "meta-autotools-multitarget: install-mapping.json missing marker: $marker" >&2
        cat "$mapping" >&2
        exit 1
    fi
done

echo "meta-autotools-multitarget: ok (4 cc rules recovered; 7 install dests captured; cross-references intact)"
