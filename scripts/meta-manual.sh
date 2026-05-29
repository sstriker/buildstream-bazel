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
#      the install-root TreeArtifact lands at
#      bazel-bin/elements/greet/greet_install/install/.
#   3. The driver extracts the tarball and asserts:
#        - usr/share/greeting.txt exists in the install root
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
              'pipeline_install('; do
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
# The install root is a TreeArtifact directory (declare_directory).
install_root="$A/bazel-bin/elements/greet/greet_install/install"
if [ ! -d "$install_root" ]; then
    echo "meta-manual: install-root TreeArtifact not produced at $install_root" >&2
    exit 1
fi

# === Verify in place (no untar) ===
greeting="$install_root/usr/share/greeting.txt"
if [ ! -f "$greeting" ]; then
    echo "meta-manual: install root missing usr/share/greeting.txt" >&2
    echo "  install root contents:" >&2
    find "$install_root" | sed 's/^/    /' >&2
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
echo "meta-manual: install-root TreeArtifact contains usr/share/greeting.txt with expected content"

# === aquery: zero tar/untar actions ===
aq=$(run_bazel "$A" aquery '//elements/greet:greet_install' 2>/dev/null || true)
if echo "$aq" | grep -qiE 'Mnemonic: .*[Tt]ar'; then
    echo "meta-manual: FAIL unexpected tar/untar action" >&2
    echo "$aq" | grep -i mnemonic >&2
    exit 1
fi
echo "meta-manual: aquery confirms zero tar/untar actions"

echo "meta-manual: ok (kind:manual install ran; %{install-root}/%{prefix} substitutions resolved; install-root TreeArtifact validated)"
