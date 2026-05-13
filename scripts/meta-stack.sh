#!/bin/sh
# meta-stack.sh — Phase 2 acceptance gate for the multi-element +
# kind:stack shape.
#
# Drives the two-pass pipeline against the two-libs fixture:
#
#   1. cmd/write-a renders project A and project B from
#      testdata/meta-project/two-libs/{lib-a,lib-b,runtime}.bst.
#      Project A gets per-element genrules for the two cmake
#      elements + a no-target marker package for the stack.
#      Project B gets the cc_library placeholders + stack's
#      filegroup composing dep labels.
#   2. bazel build in project A runs convert-element on each cmake
#      element, producing per-element BUILD.bazel.out files.
#   3. The driver stages A's BUILD.bazel.outs into B for each cmake
#      element. Stack doesn't need staging (write-a already wrote
#      its full BUILD).
#   4. bazel build //elements/runtime:runtime in B — validates the
#      stack's filegroup resolves all dep references.
#   5. The driver writes a smoke target into B (cc_binary depending
#      on both lib-a and lib-b) and runs it; output must contain
#      both libs' messages.
#
# Bazel-availability gating + META_BAZEL_*_ARGS env-var overrides
# mirror scripts/meta-hello.sh.

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

fixture="testdata/meta-project/two-libs"

"$bin_dir/write-a" \
    --bst "$fixture/lib-a.bst" \
    --bst "$fixture/lib-b.bst" \
    --bst "$fixture/runtime.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element"

# Render-phase checks. Project A: per-element BUILD for each .bst.
for f in MODULE.bazel BUILD.bazel \
        rules/zero_files.bzl tools/convert-element \
        elements/lib-a/BUILD.bazel \
        elements/lib-a/sources/CMakeLists.txt \
        elements/lib-b/BUILD.bazel \
        elements/lib-b/sources/CMakeLists.txt \
        elements/runtime/BUILD.bazel; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-stack: missing rendered project A file $f" >&2
        exit 1
    fi
done
# Stack's project-A package declares no targets (just a comment).
if grep -qE '^(filegroup|genrule|cc_library)\(' "$A/elements/runtime/BUILD.bazel"; then
    echo "meta-stack: project A's stack package should declare no targets:" >&2
    cat "$A/elements/runtime/BUILD.bazel" >&2
    exit 1
fi
# Project B: per-element packages with sources for cmake + stack
# BUILD with filegroup.
for f in MODULE.bazel BUILD.bazel \
        elements/lib-a/CMakeLists.txt \
        elements/lib-a/lib-a.c \
        elements/lib-a/include/lib-a.h \
        elements/lib-b/CMakeLists.txt \
        elements/lib-b/lib-b.c \
        elements/lib-b/include/lib-b.h \
        elements/runtime/BUILD.bazel; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-stack: missing rendered project B file $f" >&2
        exit 1
    fi
done
# Stack's project-B BUILD references both deps. Phase 3's
# buildtools-canonical formatter shortens `//pkg:pkg` to `//pkg`
# when the target name matches the package basename.
for ref in '"//elements/lib-a"' '"//elements/lib-b"'; do
    if ! grep -qF "$ref" "$B/elements/runtime/BUILD.bazel"; then
        echo "meta-stack: project B stack BUILD missing dep ref $ref" >&2
        cat "$B/elements/runtime/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-stack: render OK (3 elements: 2 cmake + 1 stack)"

# Bazel phase. Same gating as meta-hello.sh.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-stack: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-stack: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

# === Pass 1: bazel build project A's two cmake elements ===
run_bazel "$A" build //elements/lib-a:lib-a_converted //elements/lib-b:lib-b_converted 2>&1 | tail -10
for elem in lib-a lib-b; do
    out="$A/bazel-bin/elements/$elem/BUILD.bazel.out"
    if [ ! -f "$out" ]; then
        echo "meta-stack: project A's $elem BUILD.bazel.out not produced" >&2
        exit 1
    fi
    if ! grep -q '^cc_library' "$out"; then
        echo "meta-stack: $elem BUILD.bazel.out missing cc_library" >&2
        head -20 "$out" >&2
        exit 1
    fi
done
echo "meta-stack: project A built; both elements emitted cc_library declarations"

# === Stage A's outputs into B ===
for elem in lib-a lib-b; do
    cp "$A/bazel-bin/elements/$elem/BUILD.bazel.out" "$B/elements/$elem/BUILD.bazel"
    if grep -q BUILD_NOT_YET_STAGED "$B/elements/$elem/BUILD.bazel"; then
        echo "meta-stack: stage step failed for $elem" >&2
        exit 1
    fi
done

# === Validate stack's filegroup resolves ===
# This exercises the multi-element graph from B's POV: bazel resolves
# //elements/runtime:runtime's data references against the staged
# cmake elements. If write-a's stack handler emitted wrong labels,
# this fails at analysis.
run_bazel "$B" build //elements/runtime:runtime 2>&1 | tail -3
echo "meta-stack: project B //elements/runtime:runtime resolves"

# === Smoke target: cc_binary linking against both cmake elements ===
mkdir -p "$B/smoke"
cat > "$B/smoke/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "stack_smoke",
    srcs = ["smoke.c"],
    deps = [
        "//elements/lib-a:lib-a",
        "//elements/lib-b:lib-b",
    ],
)
EOF
cat > "$B/smoke/smoke.c" <<'EOF'
#include <stdio.h>
#include "lib-a.h"
#include "lib-b.h"

int main(void) {
    printf("%s\n", lib_a_message());
    printf("%s\n", lib_b_message());
    return 0;
}
EOF

run_bazel "$B" build //smoke:stack_smoke 2>&1 | tail -10
smoke_out=$(run_bazel "$B" run //smoke:stack_smoke 2>&1 | tail -10)
echo "meta-stack: smoke output:"
echo "$smoke_out" | sed 's/^/  /'
for expected in "lib-a says hi" "lib-b says hi"; do
    if ! echo "$smoke_out" | grep -qF "$expected"; then
        echo "meta-stack: smoke output missing expected line $expected" >&2
        exit 1
    fi
done

echo "meta-stack: ok (multi-element graph rendered + bazel-built; stack filegroup resolved; cc_binary linked against both cmake elements and printed both messages)"
