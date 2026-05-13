#!/bin/sh
# meta-import.sh — acceptance gate for kind:import.
#
# Drives the import-greet fixture (1 kind:import element with a
# kind:local source tree of two files) through write-a + bazel build B.
# Verifies:
#
#   1. write-a renders project A's import package as a no-target
#      marker (same shape as the other Phase-2 install-tree
#      manipulation kinds).
#   2. write-a stages the source tree into project B's
#      elements/<name>/ verbatim and renders a BUILD.bazel with a
#      filegroup over glob("**/*", exclude=["BUILD.bazel"]).
#   3. bazel build resolves //elements/greeting:greeting against
#      the staged sources, and the staged content is byte-identical
#      to the fixture's input.
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-stack.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

A="$work_dir/A"
B="$work_dir/B"

fixture="testdata/meta-project/import-greet"

"$bin_dir/write-a" \
    --bst "$fixture/greeting.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element"

# Render-phase checks: project A.
for f in MODULE.bazel BUILD.bazel \
        elements/greeting/BUILD.bazel; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-import: missing rendered project A file $f" >&2
        exit 1
    fi
done
# Import's project-A package declares no targets (just a comment).
if grep -qE '^(filegroup|genrule|cc_library)\(' "$A/elements/greeting/BUILD.bazel"; then
    echo "meta-import: project A's import package should declare no targets:" >&2
    cat "$A/elements/greeting/BUILD.bazel" >&2
    exit 1
fi
# Project B: source tree staged + BUILD.bazel with filegroup.
for f in MODULE.bazel BUILD.bazel \
        elements/greeting/BUILD.bazel \
        elements/greeting/greeting.txt \
        elements/greeting/manifest.json; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-import: missing rendered project B file $f" >&2
        exit 1
    fi
done
# Sources are staged byte-identically.
if ! cmp -s "$fixture/sources/greeting.txt" "$B/elements/greeting/greeting.txt"; then
    echo "meta-import: greeting.txt content differs from fixture source" >&2
    exit 1
fi
if ! cmp -s "$fixture/sources/manifest.json" "$B/elements/greeting/manifest.json"; then
    echo "meta-import: manifest.json content differs from fixture source" >&2
    exit 1
fi
# BUILD has the right shape.
for marker in 'name = "greeting"' \
              'glob(["**/*"], exclude = ["BUILD.bazel"])' \
              'kind:import'; do
    if ! grep -qF "$marker" "$B/elements/greeting/BUILD.bazel"; then
        echo "meta-import: project B import BUILD missing marker: $marker" >&2
        cat "$B/elements/greeting/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-import: render OK (1 import element; source tree staged into project B)"

# Bazel phase. Same gating as meta-stack.sh.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-import: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-import: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"

run_bazel() {
    workspace="$1"
    shift
    cmd="$1"
    shift
    # shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

# === Validate import's filegroup resolves ===
run_bazel "$B" build //elements/greeting:greeting 2>&1 | tail -3
echo "meta-import: project B //elements/greeting:greeting resolves"

echo "meta-import: ok (kind:import staged source tree verbatim into project B; filegroup resolves through bazel)"
