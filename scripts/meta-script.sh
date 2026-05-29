#!/bin/sh
# meta-script.sh — acceptance gate for kind:script.
#
# Drives the script-greet fixture through write-a + bazel build A:
#
#   1. cmd/write-a renders project A (kind:script genrule with the
#      config:commands list mapped onto the install-commands slot).
#      The pipelineHandler's variable resolver expands %{install-
#      root} / %{datadir} into the rendered cmd.
#   2. bazel build runs the genrule. The two commands stage
#      greeting.txt under DESTDIR/usr/share/scripts/.
#   3. The driver reads bazel-bin/elements/greet/greet_install/install/
#      and asserts:
#        - usr/share/scripts/hello.txt exists
#        - its content is "Hello from kind:script!"
#
# Bazel-availability gating mirrors the other meta-* drivers.

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

fixture="testdata/meta-project/script-greet"

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
        echo "meta-script: missing rendered project A file $f" >&2
        exit 1
    fi
done
build_path="$A/elements/greet/BUILD.bazel"
for marker in 'name = "greet_install"' \
              '# === install ===' \
              'mkdir -p $$INSTALL_ROOT/usr/share/scripts' \
              'install -D -m 0644 greeting.txt $$INSTALL_ROOT/usr/share/scripts/hello.txt' \
              'pipeline_install('; do
    if ! grep -qF -- "$marker" "$build_path"; then
        echo "meta-script: project A greet BUILD missing marker: $marker" >&2
        cat "$build_path" >&2
        exit 1
    fi
done
# Negative: no configure / build / strip phase headers — kind:script
# is install-only.
for banned in '# === configure ===' '# === build ===' '# === strip ==='; do
    if grep -qF -- "$banned" "$build_path"; then
        echo "meta-script: kind:script BUILD shouldn't have phase $banned" >&2
        exit 1
    fi
done
echo "meta-script: render OK"

# Bazel phase. Same gating as the other meta-* drivers.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-script: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-script: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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
    # shellcheck disable=SC2086
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

run_bazel "$A" build //elements/greet:greet_install 2>&1 | tail -10
# The install root is a TreeArtifact directory (declare_directory),
# read in place (no untar).
extract_dir="$A/bazel-bin/elements/greet/greet_install/install"
if [ ! -d "$extract_dir" ]; then
    echo "meta-script: install-root TreeArtifact not produced at $extract_dir" >&2
    exit 1
fi
hello="$extract_dir/usr/share/scripts/hello.txt"
if [ ! -f "$hello" ]; then
    echo "meta-script: install root missing usr/share/scripts/hello.txt" >&2
    find "$extract_dir" | sed 's/^/    /' >&2
    exit 1
fi
content=$(cat "$hello")
expected="Hello from kind:script!"
if [ "$content" != "$expected" ]; then
    echo "meta-script: hello.txt content mismatch" >&2
    echo "  want: $expected" >&2
    echo "  got:  $content" >&2
    exit 1
fi

# === aquery: zero tar/untar actions ===
aq=$(run_bazel "$A" aquery '//elements/greet:greet_install' 2>/dev/null || true)
if echo "$aq" | grep -qiE 'Mnemonic: .*[Tt]ar'; then
    echo "meta-script: FAIL unexpected tar/untar action" >&2
    echo "$aq" | grep -i mnemonic >&2
    exit 1
fi

echo "meta-script: ok (kind:script flat command list rendered + bazel-built; install-root TreeArtifact validated; zero tar/untar)"
