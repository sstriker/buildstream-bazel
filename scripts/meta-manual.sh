#!/bin/sh
# meta-manual.sh — Phase 3 acceptance gate for kind:manual.
#
# Drives the manual-greet fixture through write-a + bazel build A:
#
#   1. cmd/write-a renders project A (kind:manual gets a per-element
#      genrule whose cmd runs the .bst's phase commands and tars
#      the install root) and project B (placeholder package, see
#      assertion below).
#   2. bazel build in project A runs the manual element's genrule;
#      the install_tree.tar artifact lands at
#      bazel-bin/elements/greet/install_tree.tar.
#   3. The driver extracts the tarball and asserts:
#        - install_tree.tar/usr/share/greeting.txt exists
#        - its content is "Hello from kind:manual!"
#      That's the Phase 3 round-trip — the .bst's install-commands
#      ran, %{install-root}/%{prefix} substitutions resolved, and
#      the resulting tree was packaged for downstream consumers.
#
# A project-B-side gate (extracting the tarball into a Bazel-shaped
# wrapper for downstream cc_import / filegroup consumers) is a
# follow-up: the install-tree-as-typed-filegroups shape needs the
# variable parser + multi-element fixtures with manual elements as
# parents to land first.
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
trap "rm -rf '$work_dir'" EXIT

A="$work_dir/A"
B="$work_dir/B"

fixture="testdata/meta-project/manual-greet"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks.
for f in MODULE.bazel BUILD.bazel \
        elements/greet/BUILD.bazel \
        elements/greet/sources/greeting.txt; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-manual: missing rendered project A file $f" >&2
        exit 1
    fi
done
# Project A's manual element BUILD declares an install genrule with
# the substituted variable references the handler emits.
for marker in 'name = "greet_install"' \
              '# === install ===' \
              '$$INSTALL_ROOT/usr/share/greeting.txt' \
              'outs = ["install_tree.tar"]'; do
    if ! grep -qF "$marker" "$A/elements/greet/BUILD.bazel"; then
        echo "meta-manual: project A greet BUILD missing marker: $marker" >&2
        cat "$A/elements/greet/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-manual: render OK"

# Bazel phase. Same gating as meta-hello / meta-stack.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-manual: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-manual: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

# === bazel build project A ===
run_bazel "$A" build //elements/greet:greet_install 2>&1 | tail -10
install_tar="$A/bazel-bin/elements/greet/install_tree.tar"
if [ ! -f "$install_tar" ]; then
    echo "meta-manual: install_tree.tar not produced" >&2
    exit 1
fi

# === Extract + verify ===
extract_dir="$work_dir/extract"
mkdir -p "$extract_dir"
tar -xf "$install_tar" -C "$extract_dir"
greeting="$extract_dir/usr/share/greeting.txt"
if [ ! -f "$greeting" ]; then
    echo "meta-manual: extracted tarball missing usr/share/greeting.txt" >&2
    echo "  tarball contents:" >&2
    tar -tf "$install_tar" | sed 's/^/    /' >&2
    exit 1
fi
content=$(cat "$greeting")
expected="Hello from kind:manual!"
if [ "$content" != "$expected" ]; then
    echo "meta-manual: greeting.txt content mismatch" >&2
    echo "  want: $expected" >&2
    echo "  got:  $content" >&2
    exit 1
fi
echo "meta-manual: install_tree.tar contains usr/share/greeting.txt with expected content"

echo "meta-manual: ok (kind:manual genrule ran; %{install-root}/%{prefix} substitutions resolved; install tarball validated)"
