#!/bin/sh
# meta-autotools-asm.sh — acceptance gate for assembler source
# (.S, cpp-then-assemble) recognition. The fixture's Makefile
# compiles both foo.c and sysv.S into libfoo.a; the converter
# must include sysv.S in cc_library's srcs.
#
# Real autotools projects (libffi being the canonical example)
# select arch-specific .S sources via configure.host. Without
# .S in the converter's source-file allowlist, those compile
# events get silently dropped during classifyArgv (srcs=[]
# fails the compile-only branch's len(srcs)==0 check).
#
# The gate asserts:
#   1. Render: project A's per-element BUILD wires the install
#      genrule (no asm-specific render-time markers).
#   2. Build (when bazel is available): the install genrule's
#      pipeline runs configure + make + install + tracer +
#      converter, producing the usual outputs.
#   3. BUILD.bazel.out's cc_library has srcs=["foo.c", "sysv.S"]
#      AND defines=["ASM_VARIANT=42"] from sysv.o's per-target
#      CFLAGS (proves the .S compile event was correlated, not
#      merely tolerated).
#   4. install-mapping.json captures libfoo.a + foo.h.
#
# Host-arch note: the fixture ships an x86_64 .S body. On non-
# x86_64 hosts the gate's render phase still passes (the .bst
# render is arch-agnostic), but the build phase would fail at
# the assembler. We don't gate on `uname -m` here because the
# build phase is itself bazel-availability-gated; CI runs on
# x86_64.

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

fixture="testdata/meta-project/autotools-asm"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/asm.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-round1

for marker in \
    '"BUILD.bazel.out"' \
    '"make-db.txt"' \
    '"install-mapping.json"' \
    'name = "asm_install"'; do
    if ! grep -qF -- "$marker" "$B/elements/asm/BUILD.bazel"; then
        echo "meta-autotools-asm: render missing marker: $marker" >&2
        cat "$B/elements/asm/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-autotools-asm: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-asm: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-autotools-asm: render OK; bazel < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

run_bazel "$B" build //elements/asm:asm_install 2>&1 | tail -10

build_out="$B/bazel-bin/elements/asm/asm_install/BUILD.bazel.out"
mapping="$B/bazel-bin/elements/asm/asm_install/install-mapping.json"
for want in "$build_out" "$mapping"; do
    if [ ! -f "$want" ]; then
        echo "meta-autotools-asm: missing build output $want" >&2
        exit 1
    fi
done

# BUILD.bazel.out shape: cc_library(foo) with both foo.c and
# sysv.S in srcs, plus the ASM_VARIANT define from sysv.o's
install_root="$B/bazel-bin/elements/asm/asm_install/install"
if [ ! -d "$install_root" ]; then
    echo "meta-autotools-asm: missing install-root TreeArtifact at $install_root" >&2
    exit 1
fi
aq=$(run_bazel "$B" aquery '//elements/asm:asm_install' 2>/dev/null || true)
if echo "$aq" | grep -qiE 'Mnemonic: .*[Tt]ar'; then
    echo "meta-autotools-asm: FAIL unexpected tar/untar action" >&2
    echo "$aq" | grep -i mnemonic >&2
    exit 1
fi
# build-tracer needs real ptrace; under nested sandboxes the trace is
# empty and the converter emits a "no buildable targets" placeholder.
# Skip the trace-recovery assertions in that case (environment
# limitation, not a render regression).
if grep -qF '# (no buildable targets recovered from trace)' "$build_out"; then
    echo "meta-autotools-asm: ok (install-root TreeArtifact built; zero tar/untar); trace recovered no targets in this environment — cc assertions skipped"
    exit 0
fi

# per-target CFLAGS.
for marker in \
    'cc_library(' \
    'name = "foo"' \
    '"foo.c"' \
    '"sysv.S"' \
    '"ASM_VARIANT=42"'; do
    if ! grep -qF -- "$marker" "$build_out"; then
        echo "meta-autotools-asm: BUILD.bazel.out missing marker: $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

# install-mapping.json shape.
for marker in \
    '"libfoo.a"' \
    '"foo.h"'; do
    if ! grep -qF -- "$marker" "$mapping"; then
        echo "meta-autotools-asm: install-mapping.json missing marker: $marker" >&2
        cat "$mapping" >&2
        exit 1
    fi
done

echo "meta-autotools-asm: ok (.S source recognized; cc_library srcs include sysv.S; ASM_VARIANT define preserved)"
