#!/bin/sh
# meta-bazel-passthrough.sh — acceptance gate for kind:bazel.
#
# kind:bazel is a passthrough: the element's source tree already
# contains BUILD files; write-a stages them verbatim into project
# B and runs no per-kind translator. The gate asserts:
#
#   1. Project A's elements/<name>/BUILD.bazel is a no-target
#      marker (no genrule / cc_library / filegroup).
#   2. Project B's elements/<name>/BUILD.bazel is the source
#      tree's authored BUILD verbatim (byte-identical to the
#      fixture's BUILD.bazel).
#   3. bazel build over project B's element succeeds (the
#      authored BUILD's cc_binary compiles + runs + prints the
#      expected output).
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-hello.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

fixture="testdata/meta-project/bazel-passthrough"

"$bin_dir/write-a" \
    --bst "$fixture/passthrough.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element"

# Render-phase asserts.
if grep -qE '(genrule|cc_library|cc_binary|filegroup)\(' "$A/elements/passthrough/BUILD.bazel"; then
    echo "meta-bazel-passthrough: project A BUILD should be a no-target marker" >&2
    cat "$A/elements/passthrough/BUILD.bazel" >&2
    exit 1
fi
if ! diff -q \
    "$fixture/sources/BUILD.bazel" \
    "$B/elements/passthrough/BUILD.bazel" >/dev/null; then
    echo "meta-bazel-passthrough: project B BUILD diverges from authored source BUILD" >&2
    diff -u "$fixture/sources/BUILD.bazel" "$B/elements/passthrough/BUILD.bazel" >&2
    exit 1
fi
echo "meta-bazel-passthrough: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-bazel-passthrough: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-bazel-passthrough: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

# Pass: build the passthrough cc_binary directly out of project B.
run_bazel "$B" build //elements/passthrough:passthrough 2>&1 | tail -10
out=$(run_bazel "$B" run //elements/passthrough:passthrough 2>&1 | tail -3)
echo "$out" | grep -q "kind:bazel passthrough OK" || {
    echo "meta-bazel-passthrough: smoke binary output unexpected:" >&2
    echo "$out" >&2
    exit 1
}
echo "meta-bazel-passthrough: ok (source-authored BUILD ran end-to-end through bazel build)"
