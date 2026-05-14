#!/bin/sh
# meta-autotools-native.sh — end-to-end acceptance gate for the
# trace-driven kind:autotools native render path.
#
# Drives the full single-genrule design end-to-end:
#
#   1. cmd/write-a renders project A with --convert-element-trace
#      + --build-tracer-bin set. kind:autotools elements get a single
#      install genrule with two outputs (install_tree.tar + the
#      native BUILD.bazel.out) and tools = [build-tracer,
#      convert-element-trace].
#   2. bazel build runs the genrule once. Inside the sandbox:
#      build-tracer wraps the configure/build/install pipeline,
#      capturing every execve into a trace file; convert-element-
#      trace reads the trace and emits BUILD.bazel.out with
#      native cc_library / cc_binary targets.
#   3. The driver extracts BUILD.bazel.out from project A's
#      bazel-bin and asserts the native shape (cc_binary or
#      cc_library, depending on the fixture's pipeline outputs).
#
# Bazel's action cache (buildbarn in CI) handles cross-node
# convergence transparently — same source + same toolchain + same
# converter version => same action result, shared via the existing
# remote-cache plumbing.
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-autotools.sh.

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

fixture="testdata/meta-project/autotools-greet"

"$bin_dir/write-a" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-round1

# Render-phase checks.
for want in \
    "elements/greet/BUILD.bazel" \
    "tools/convert-element-trace" \
    "tools/build-tracer"; do
    if [ ! -f "$A/$want" ]; then
        echo "meta-autotools-native: missing rendered file $want in project A" >&2
        exit 1
    fi
done
for marker in \
    '"BUILD.bazel.out"' \
    '"//tools:build-tracer"' \
    '"//tools:convert-element-trace"' \
    'name = "greet_install"'; do
    if ! grep -qF -- "$marker" "$B/elements/greet/BUILD.bazel"; then
        echo "meta-autotools-native: rendered BUILD missing marker: $marker" >&2
        cat "$B/elements/greet/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-autotools-native: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-native: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-autotools-native: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi
if ! command -v strace >/dev/null; then
    echo "meta-autotools-native: render OK; strace not on PATH (build-tracer needs it), skipping build phase" >&2
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

# Single-genrule shape: one bazel build runs build-tracer
# wrapping configure/make/make install + the inline
# convert-element-trace call. Outputs:
# install_tree.tar + BUILD.bazel.out + make-db.txt +
# install-mapping.json — all in one action, one cache key.
#
# build-tracer's native backend uses ptrace from a parent
# process; bazel's default linux-sandbox usually allows that
# (kernel.yama.ptrace_scope = 1 means "trace your own
# children"), so we don't need --spawn_strategy=local.
run_bazel "$B" build //elements/greet:greet_install 2>&1 | tail -10

# Native BUILD.bazel.out + install_tree.tar should both exist.
for want in \
    "$B/bazel-bin/elements/greet/install_tree.tar" \
    "$B/bazel-bin/elements/greet/BUILD.bazel.out"; do
    if [ ! -f "$want" ]; then
        echo "meta-autotools-native: missing build output $want" >&2
        exit 1
    fi
done

# Native shape: BUILD.bazel.out should declare a cc_binary
# matching the fixture's `greet` binary.
build_out="$B/bazel-bin/elements/greet/BUILD.bazel.out"
for marker in \
    'load("@rules_cc//cc:defs.bzl"' \
    'cc_binary(' \
    'name = "greet"' \
    'srcs = ["greet.c"]'; do
    if ! grep -qF "$marker" "$build_out"; then
        echo "meta-autotools-native: BUILD.bazel.out missing $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

echo "meta-autotools-native: ok (tracer-wrapped install + native converter ran inline; produced cc_binary(greet))"
echo "--- BUILD.bazel.out ---"
cat "$build_out"
