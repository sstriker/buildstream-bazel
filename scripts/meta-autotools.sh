#!/bin/sh
# meta-autotools.sh — acceptance gate for kind:autotools.
#
# Drives the autotools-greet fixture through write-a + bazel build A:
#
#   1. cmd/write-a renders project A's autotools genrule. The
#      pipelineHandler defaults expand the BuildStream autotools
#      plugin's canonical %{autogen} / %{configure} / %{make} /
#      %{make-install} chain through the variable resolver; project.conf
#      sets prefix=/usr so the rendered ./configure invocation lands
#      under /usr.
#   2. bazel build runs the genrule. Inside the sandbox: autogen
#      finds ./configure (no regeneration needed), the canonical
#      autoconf flags pass through, configure substitutes @PREFIX@
#      into Makefile.in, make compiles greet, make install places
#      the binary under DESTDIR/usr/bin.
#   3. The driver extracts bazel-bin/elements/greet/install_tree.tar
#      and asserts:
#        - usr/bin/greet exists and is executable.
#        - Running it prints "greet from kind:autotools".
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-make.sh.

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

fixture="testdata/meta-project/autotools-greet"

"$bin_dir/write-a" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element"

# Render-phase checks.
for f in MODULE.bazel BUILD.bazel \
        elements/greet/BUILD.bazel \
        elements/greet/sources/configure \
        elements/greet/sources/Makefile.in \
        elements/greet/sources/greet.c; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-autotools: missing rendered project A file $f" >&2
        exit 1
    fi
done
# Project A's autotools BUILD declares the install genrule + has the
# resolved autoconf flag set inlined (project.conf prefix=/usr drives
# every derived flag).
for marker in 'name = "greet_install"' \
              '# === configure ===' \
              '# === build ===' \
              '# === install ===' \
              '--prefix=/usr' \
              '--bindir=/usr/bin' \
              '--libdir=/usr/lib' \
              'make -j1 DESTDIR="$$INSTALL_ROOT" install' \
              'outs = ["install_tree.tar"]'; do
    if ! grep -qF -- "$marker" "$A/elements/greet/BUILD.bazel"; then
        echo "meta-autotools: project A greet BUILD missing marker: $marker" >&2
        cat "$A/elements/greet/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-autotools: render OK"

# Bazel phase. Same gating as meta-make.sh.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-autotools: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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
    echo "meta-autotools: install_tree.tar not produced" >&2
    exit 1
fi

# === Extract + verify ===
extract_dir="$work_dir/extract"
mkdir -p "$extract_dir"
tar -xf "$install_tar" -C "$extract_dir"
greet_bin="$extract_dir/usr/bin/greet"
if [ ! -x "$greet_bin" ]; then
    echo "meta-autotools: extracted tarball missing executable usr/bin/greet" >&2
    echo "  tarball contents:" >&2
    tar -tf "$install_tar" | sed 's/^/    /' >&2
    exit 1
fi
runtime_out=$("$greet_bin")
expected="greet from kind:autotools"
if [ "$runtime_out" != "$expected" ]; then
    echo "meta-autotools: greet runtime output mismatch" >&2
    echo "  want: $expected" >&2
    echo "  got:  $runtime_out" >&2
    exit 1
fi
echo "meta-autotools: install_tree.tar contains usr/bin/greet that runs and prints expected output"

echo "meta-autotools: ok (autotools defaults expanded; ./configure honored canonical autoconf flags; make compiled greet.c; install placed binary; runtime output validated)"
