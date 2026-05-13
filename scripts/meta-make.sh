#!/bin/sh
# meta-make.sh — Phase 3 acceptance gate for kind:make.
#
# Drives the make-greet fixture through write-a + bazel build A:
#
#   1. cmd/write-a renders project A (kind:make uses the
#      pipelineHandler shape with `make` / `make ... install`
#      defaults, so the .bst doesn't need a config: block) and
#      project B (placeholder package).
#   2. bazel build in project A runs the manual element's genrule;
#      the install_tree.tar artifact lands at
#      bazel-bin/elements/greet/install_tree.tar.
#   3. The driver extracts the tarball and asserts:
#        - usr/bin/greet exists and is executable
#        - running it prints "greet from kind:make"
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
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element"

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
install_tar="$A/bazel-bin/elements/greet/install_tree.tar"
if [ ! -f "$install_tar" ]; then
    echo "meta-make: install_tree.tar not produced" >&2
    exit 1
fi

# === Extract + verify ===
extract_dir="$work_dir/extract"
mkdir -p "$extract_dir"
tar -xf "$install_tar" -C "$extract_dir"
greet="$extract_dir/usr/bin/greet"
if [ ! -x "$greet" ]; then
    echo "meta-make: extracted tarball missing executable usr/bin/greet" >&2
    echo "  tarball contents:" >&2
    tar -tf "$install_tar" | sed 's/^/    /' >&2
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
echo "meta-make: install_tree.tar contains usr/bin/greet that runs and prints expected output"

echo "meta-make: ok (kind:make defaults resolved; make compiled greet.c; install placed binary; runtime output validated)"
