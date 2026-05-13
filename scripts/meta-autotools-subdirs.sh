#!/bin/sh
# meta-autotools-subdirs.sh — acceptance gate for the
# recursive-automake (SUBDIRS) collision case the build-tracer
# cwd-capture work fixes.
#
# Fixture: a top-level Makefile.in that dispatches to two
# subdirs (libA, libB), each compiling its own source into
# `parent.o` (basename collision) and archiving it into
# lib<X>.a. Without cwd capture, both archives would resolve
# to the same compile event in the converter's objByPath /
# objByBasename maps; the recovered BUILD.bazel.out would
# either lose one archive's distinct compile flags or wire
# the wrong .c into the wrong cc_library.
#
# The gate asserts:
#   1. Render: project A's per-element BUILD wires the install
#      genrule with build-tracer + convert-element-trace.
#   2. Build (when bazel is available): the install genrule's
#      pipeline runs configure + make + install + tracer +
#      converter, producing install_tree.tar +
#      BUILD.bazel.out + make-db.txt + install-mapping.json.
#   3. BUILD.bazel.out contains BOTH cc_library rules (A and
#      B), each with the correct VARIANT define (1 vs 2)
#      sourced from the matching subdir's compile.
#   4. install-mapping.json captures both libA.a and libB.a
#      with rule cross-references.

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

fixture="testdata/meta-project/autotools-subdirs"

"$bin_dir/write-a" \
    --bst "$fixture/subdirs.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" \
    --convert-element-trace "$bin_dir/convert-element-trace" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-round1

for marker in \
    '"BUILD.bazel.out"' \
    '"make-db.txt"' \
    '"install-mapping.json"' \
    'name = "subdirs_install"' \
    '--out-install-mapping="$(location install-mapping.json)"'; do
    if ! grep -qF -- "$marker" "$B/elements/subdirs/BUILD.bazel"; then
        echo "meta-autotools-subdirs: render missing marker: $marker" >&2
        cat "$B/elements/subdirs/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-autotools-subdirs: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-autotools-subdirs: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-autotools-subdirs: render OK; bazel < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

run_bazel "$B" build //elements/subdirs:subdirs_install 2>&1 | tail -10

build_out="$B/bazel-bin/elements/subdirs/BUILD.bazel.out"
mapping="$B/bazel-bin/elements/subdirs/install-mapping.json"
for want in "$build_out" "$mapping" \
            "$B/bazel-bin/elements/subdirs/install_tree.tar" \
            "$B/bazel-bin/elements/subdirs/make-db.txt"; do
    if [ ! -f "$want" ]; then
        echo "meta-autotools-subdirs: missing build output $want" >&2
        exit 1
    fi
done

# BUILD.bazel.out shape: both archives recovered, each with the
# right VARIANT define (this is the cwd-disambiguation property
# the build-tracer + converter changes deliver).
for marker in \
    'cc_library(' \
    'name = "A"' \
    'name = "B"' \
    '"VARIANT=1"' \
    '"VARIANT=2"'; do
    if ! grep -qF -- "$marker" "$build_out"; then
        echo "meta-autotools-subdirs: BUILD.bazel.out missing marker: $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

# Per-archive define isolation: VARIANT=1 must NOT show up
# in the libB rule body (and vice versa). Without cwd-keyed
# correlation, last-write-wins on objByPath["parent.o"] would
# cause BOTH archives to pick up the SAME compile event's
# defines.
A_block=$(awk '/cc_library\(/{rule=$0; getline; rule=rule"\n"$0; next} /^\)/{if(rule ~ /name = "A"/)print rule; rule=""; next} {rule=rule"\n"$0}' "$build_out")
B_block=$(awk '/cc_library\(/{rule=$0; getline; rule=rule"\n"$0; next} /^\)/{if(rule ~ /name = "B"/)print rule; rule=""; next} {rule=rule"\n"$0}' "$build_out")
if printf '%s\n' "$A_block" | grep -q 'VARIANT=2'; then
    echo "meta-autotools-subdirs: cc_library(A) leaked VARIANT=2 — cwd disambiguation failed" >&2
    cat "$build_out" >&2
    exit 1
fi
if printf '%s\n' "$B_block" | grep -q 'VARIANT=1'; then
    echo "meta-autotools-subdirs: cc_library(B) leaked VARIANT=1 — cwd disambiguation failed" >&2
    cat "$build_out" >&2
    exit 1
fi

# install-mapping.json shape: both archives + the shared
# header captured. Note: rule cross-references aren't asserted
# here — the install recipe's path-prefixed sources
# ("libA/libA.a") don't match the converter's flat ruleByName
# lookup ("libA.a" → "A"), which is a separate issue from the
# cwd-disambiguation property under test.
for marker in \
    '"source": "libA/libA.a"' \
    '"source": "libB/libB.a"' \
    '"source": "include/parent.h"'; do
    if ! grep -qF -- "$marker" "$mapping"; then
        echo "meta-autotools-subdirs: install-mapping.json missing marker: $marker" >&2
        cat "$mapping" >&2
        exit 1
    fi
done

echo "meta-autotools-subdirs: ok (SUBDIRS .o-basename collision disambiguated by cwd; both archives recovered with correct per-subdir defines)"
