#!/bin/sh
# meta-bazel-override.sh — acceptance gate for --build-files-dir.
#
# Tests the per-element BUILD override directory: a kind:manual
# .bst paired with a sibling build-files/<name>.BUILD.bazel gets
# re-stamped by write-a to kind:bazel, and the supplied BUILD
# becomes project B's elements/<name>/BUILD.bazel. Sources stage
# alongside so the override's srcs = [...] resolves.
#
# Asserts:
#   1. Project A's elements/<name>/BUILD.bazel is a no-target
#      marker (kind:bazel's project-A shape — no genrule fired).
#   2. Project B's elements/<name>/BUILD.bazel carries the
#      operator's cc_binary verbatim (modulo writeFile's
#      buildifier canonicalization).
#   3. The element's kind:local source tree staged alongside
#      (greet.c lands in project B).
#   4. bazel build over project B's element succeeds (the
#      cc_binary compiles + runs + prints the expected output).
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-bazel-passthrough.sh.

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

fixture="testdata/meta-project/bazel-override"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/override.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --build-files-dir "$fixture/build-files"

# Render-phase asserts.
if grep -qE '(genrule|cc_library)\(' "$A/elements/override/BUILD.bazel"; then
    echo "meta-bazel-override: project A BUILD should be a no-target marker (the kind:manual handler's genrule must not fire)" >&2
    cat "$A/elements/override/BUILD.bazel" >&2
    exit 1
fi
if ! grep -q 'cc_binary(' "$B/elements/override/BUILD.bazel"; then
    echo "meta-bazel-override: project B BUILD didn't carry the override's cc_binary" >&2
    cat "$B/elements/override/BUILD.bazel" >&2
    exit 1
fi
if ! grep -q '"greet.c"' "$B/elements/override/BUILD.bazel"; then
    echo "meta-bazel-override: project B BUILD didn't carry the override's srcs = [\"greet.c\"]" >&2
    cat "$B/elements/override/BUILD.bazel" >&2
    exit 1
fi
if [ ! -f "$B/elements/override/greet.c" ]; then
    echo "meta-bazel-override: kind:local source not staged alongside the override BUILD" >&2
    ls -la "$B/elements/override/" >&2
    exit 1
fi
echo "meta-bazel-override: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-bazel-override: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-bazel-override: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

run_bazel "$B" build //elements/override:override 2>&1 | tail -10
out=$(run_bazel "$B" run //elements/override:override 2>&1 | tail -3)
echo "$out" | grep -q "kind:bazel override OK" || {
    echo "meta-bazel-override: smoke binary output unexpected:" >&2
    echo "$out" >&2
    exit 1
}
echo "meta-bazel-override: ok (operator-supplied BUILD ran end-to-end through bazel build)"
