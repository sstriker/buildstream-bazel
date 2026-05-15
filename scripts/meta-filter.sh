#!/bin/sh
# meta-filter.sh — acceptance gate for kind:filter.
#
# Drives the filter-greet fixture (1 cmake parent + 1 filter) through
# write-a + bazel build A. Verifies:
#
#   1. write-a renders project A's filter package as a no-target
#      marker (same shape as kind:stack / kind:compose).
#   2. write-a renders project B's filter BUILD as a single-dep
#      filegroup whose data references the parent (//elements/greet:greet).
#   3. The .bst's `config: include: [public-headers]` is recorded
#      as a comment inside the rendered BUILD (domain enforcement
#      itself is deferred — see handler_filter.go).
#   4. bazel build resolves //elements/greet-headers:greet-headers
#      against the staged cmake parent, and a smoke cc_binary
#      depending on the filter target compiles + runs.
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

fixture="testdata/meta-project/filter-greet"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/greet.bst" \
    --bst "$fixture/greet-headers.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks: project A.
for f in MODULE.bazel BUILD.bazel \
        elements/greet/BUILD.bazel \
        elements/greet-headers/BUILD.bazel; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-filter: missing rendered project A file $f" >&2
        exit 1
    fi
done
# Filter's project-A package declares no targets.
if grep -qE '^(filegroup|genrule|cc_library)\(' "$A/elements/greet-headers/BUILD.bazel"; then
    echo "meta-filter: project A's filter package should declare no targets:" >&2
    cat "$A/elements/greet-headers/BUILD.bazel" >&2
    exit 1
fi
# Project B: cmake parent + filter package.
for f in MODULE.bazel BUILD.bazel \
        elements/greet/CMakeLists.txt \
        elements/greet-headers/BUILD.bazel; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-filter: missing rendered project B file $f" >&2
        exit 1
    fi
done
# Filter's BUILD references the single dep + records the config:include
# domains as a comment.
for marker in 'name = "greet-headers"' \
              '"//elements/greet:greet"' \
              'kind:filter' \
              '# include domains: [public-headers]'; do
    if ! grep -qF "$marker" "$B/elements/greet-headers/BUILD.bazel"; then
        echo "meta-filter: project B filter BUILD missing marker: $marker" >&2
        cat "$B/elements/greet-headers/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-filter: render OK (2 elements: 1 cmake + 1 filter)"

# Bazel phase. Same gating as meta-stack.sh.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-filter: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-filter: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

# === Pass 1: bazel build the cmake parent ===
run_bazel "$A" build //elements/greet:greet_converted 2>&1 | tail -10
out="$A/bazel-bin/elements/greet/BUILD.bazel.out"
if [ ! -f "$out" ]; then
    echo "meta-filter: project A's greet BUILD.bazel.out not produced" >&2
    exit 1
fi
cp "$out" "$B/elements/greet/BUILD.bazel"

# === Validate filter's filegroup resolves ===
run_bazel "$B" build //elements/greet-headers:greet-headers 2>&1 | tail -3
echo "meta-filter: project B //elements/greet-headers:greet-headers resolves"

# === Smoke target: cc_binary depending on the filter target ===
mkdir -p "$B/smoke"
cat > "$B/smoke/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "filter_smoke",
    srcs = ["smoke.c"],
    deps = ["//elements/greet-headers:greet-headers"],
)
EOF
cat > "$B/smoke/smoke.c" <<'EOF'
#include <stdio.h>
#include "greet.h"

int main(void) {
    printf("%s\n", greet_message());
    return 0;
}
EOF

run_bazel "$B" build //smoke:filter_smoke 2>&1 | tail -10
smoke_out=$(run_bazel "$B" run //smoke:filter_smoke 2>&1 | tail -10)
if ! echo "$smoke_out" | grep -qF "greet from kind:filter"; then
    echo "meta-filter: smoke output missing expected line:" >&2
    echo "$smoke_out" | sed 's/^/  /' >&2
    exit 1
fi

echo "meta-filter: ok (kind:filter renders a single-dep filegroup; config:include recorded; cc_binary linked through the filter target)"
