#!/bin/sh
# meta-compose.sh — acceptance gate for kind:compose.
#
# Drives the compose-greet fixture through write-a + bazel build A:
# two kind:cmake parents (greet-a, greet-b) plus one kind:compose
# bundle composing them. Verifies the rendering shape matches kind:stack
# (no project-A action; project B filegroup over dep labels) and that
# the bundle resolves end-to-end through bazel.
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

fixture="testdata/meta-project/compose-greet"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/greet-a.bst" \
    --bst "$fixture/greet-b.bst" \
    --bst "$fixture/bundle.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks: project A. rules/ is no longer rendered
# (loads from @rules_buildstream_bazel//rules).
for f in MODULE.bazel BUILD.bazel \
        tools/convert-element-cmake \
        elements/greet-a/BUILD.bazel \
        elements/greet-b/BUILD.bazel \
        elements/bundle/BUILD.bazel; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-compose: missing rendered project A file $f" >&2
        exit 1
    fi
done
# Compose's project-A package declares no targets (just a comment).
if grep -qE '^(filegroup|genrule|cc_library)\(' "$A/elements/bundle/BUILD.bazel"; then
    echo "meta-compose: project A's compose package should declare no targets:" >&2
    cat "$A/elements/bundle/BUILD.bazel" >&2
    exit 1
fi
# Project B: per-element packages with sources for cmake + compose
# BUILD with filegroup.
for f in MODULE.bazel BUILD.bazel \
        elements/greet-a/CMakeLists.txt \
        elements/greet-b/CMakeLists.txt \
        elements/bundle/BUILD.bazel; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-compose: missing rendered project B file $f" >&2
        exit 1
    fi
done
# Compose's project-B BUILD references both deps.
for ref in '"//elements/greet-a:greet-a"' '"//elements/greet-b:greet-b"'; do
    if ! grep -qF "$ref" "$B/elements/bundle/BUILD.bazel"; then
        echo "meta-compose: project B compose BUILD missing dep ref $ref" >&2
        cat "$B/elements/bundle/BUILD.bazel" >&2
        exit 1
    fi
done
# kind:compose comment marker confirms we hit the right handler (vs
# kind:stack, which renders a near-identical filegroup).
if ! grep -qF "kind:compose" "$B/elements/bundle/BUILD.bazel"; then
    echo "meta-compose: project B BUILD should be tagged with kind:compose marker" >&2
    cat "$B/elements/bundle/BUILD.bazel" >&2
    exit 1
fi
echo "meta-compose: render OK (3 elements: 2 cmake + 1 compose)"

# Bazel phase. Same gating as meta-stack.sh.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-compose: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-compose: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi

# Bazel-build half execs convert-element-cmake which shells to
# cmake + ninja under bwrap. Skip cleanly if any are missing.
for tool in cmake ninja bwrap; do
    if ! command -v "$tool" >/dev/null; then
        echo "meta-compose: render OK; $tool not on PATH, skipping build phase"
        exit 0
    fi
done

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
run_bazel "$A" build //elements/greet-a:greet-a_converted //elements/greet-b:greet-b_converted 2>&1 | tail -10
for elem in greet-a greet-b; do
    out="$A/bazel-bin/elements/$elem/BUILD.bazel.out"
    if [ ! -f "$out" ]; then
        echo "meta-compose: project A's $elem BUILD.bazel.out not produced" >&2
        exit 1
    fi
    if ! grep -q '^cc_library' "$out"; then
        echo "meta-compose: $elem BUILD.bazel.out missing cc_library" >&2
        head -20 "$out" >&2
        exit 1
    fi
done

# === Stage A's outputs into B ===
for elem in greet-a greet-b; do
    cp "$A/bazel-bin/elements/$elem/BUILD.bazel.out" "$B/elements/$elem/BUILD.bazel"
done

# === Validate compose's filegroup resolves ===
run_bazel "$B" build //elements/bundle:bundle 2>&1 | tail -3
echo "meta-compose: project B //elements/bundle:bundle resolves"

# === Smoke target: cc_binary linking against both cmake elements ===
mkdir -p "$B/smoke"
cat > "$B/smoke/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "compose_smoke",
    srcs = ["smoke.c"],
    deps = [
        "//elements/greet-a:greet-a",
        "//elements/greet-b:greet-b",
    ],
)
EOF
cat > "$B/smoke/smoke.c" <<'EOF'
#include <stdio.h>
#include "greet-a.h"
#include "greet-b.h"

int main(void) {
    printf("%s\n", greet_a_message());
    printf("%s\n", greet_b_message());
    return 0;
}
EOF

run_bazel "$B" build //smoke:compose_smoke 2>&1 | tail -10
smoke_out=$(run_bazel "$B" run //smoke:compose_smoke 2>&1 | tail -10)
for expected in "greet-a from compose" "greet-b from compose"; do
    if ! echo "$smoke_out" | grep -qF "$expected"; then
        echo "meta-compose: smoke output missing expected line: $expected" >&2
        exit 1
    fi
done

echo "meta-compose: ok (kind:compose renders the same filegroup-over-deps shape as kind:stack; cc_binary linked against both elements via the compose target)"
