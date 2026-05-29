#!/bin/sh
# meta-make.sh — Phase 3 acceptance gate for kind:make.
#
# Drives the make-greet fixture through write-a + bazel build A:
#
#   1. cmd/write-a renders project A (kind:make uses the
#      pipelineHandler shape with `make` / `make ... install`
#      defaults, so the .bst doesn't need a config: block) and
#      project B (placeholder package).
#   2. bazel build in project A runs the element's pipeline_install;
#      the install-root TreeArtifact lands at
#      bazel-bin/elements/greet/greet_install/install/.
#   3. The driver reads the directory in place (no untar) and asserts:
#        - usr/bin/greet exists and is executable
#        - running it prints "greet from kind:make"
#        - aquery shows PipelineInstall + zero tar/untar actions
#      That's the round-trip — `make` compiled greet.c, `make install`
#      placed the binary, both kind:make defaults resolved.
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-manual.sh.

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

fixture="testdata/meta-project/make-greet"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks.
for f in MODULE.bazel BUILD.bazel \
        elements/greet/BUILD.bazel \
        elements/greet/sources/Makefile \
        elements/greet/sources/greet.c; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-make: missing rendered project A file $f" >&2
        exit 1
    fi
done
# kind:make's defaults render in the cmd: build phase runs `make`,
# install phase runs `make -j1 DESTDIR=... install`. No explicit
# .bst config:, so the renderer falls back to the handler defaults.
for marker in 'name = "greet_install"' \
              '# === build ===' \
              '        make' \
              '# === install ===' \
              'make -j1 DESTDIR="$$INSTALL_ROOT" install'; do
    if ! grep -qF "$marker" "$A/elements/greet/BUILD.bazel"; then
        echo "meta-make: project A greet BUILD missing marker: $marker" >&2
        cat "$A/elements/greet/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-make: render OK"

# Bazel phase. Same gating as the other meta-* drivers.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-make: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-make: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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
# The install root is a TreeArtifact directory (declare_directory),
# not an opaque install_tree.tar. pipeline_install declares it at
# <name>/install under the target's package output dir.
install_root="$A/bazel-bin/elements/greet/greet_install/install"
if [ ! -d "$install_root" ]; then
    echo "meta-make: install-root TreeArtifact not produced at $install_root" >&2
    exit 1
fi

# === Verify in place (no untar) ===
greet="$install_root/usr/bin/greet"
if [ ! -x "$greet" ]; then
    echo "meta-make: install root missing executable usr/bin/greet" >&2
    echo "  install root contents:" >&2
    find "$install_root" | sed 's/^/    /' >&2
    exit 1
fi
output=$("$greet")
expected="greet from kind:make"
if [ "$output" != "$expected" ]; then
    echo "meta-make: greet binary output mismatch" >&2
    echo "  want: $expected" >&2
    echo "  got:  $output" >&2
    exit 1
fi
echo "meta-make: install-root TreeArtifact contains usr/bin/greet that runs and prints expected output"

# === aquery: confirm zero tar/untar actions in the install graph ===
aq=$(run_bazel "$A" aquery '//elements/greet:greet_install' 2>/dev/null || true)
if echo "$aq" | grep -qiE 'Mnemonic: .*[Tt]ar'; then
    echo "meta-make: FAIL unexpected tar/untar action in install graph" >&2
    echo "$aq" | grep -i mnemonic >&2
    exit 1
fi
if ! echo "$aq" | grep -q 'Mnemonic: PipelineInstall'; then
    echo "meta-make: FAIL expected a PipelineInstall action in the graph" >&2
    echo "$aq" | grep -i mnemonic >&2
    exit 1
fi
echo "meta-make: aquery confirms PipelineInstall + zero tar/untar actions"

echo "meta-make: ok (kind:make defaults resolved; make compiled greet.c; install placed binary into TreeArtifact root; runtime output validated)"
