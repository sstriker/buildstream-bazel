#!/bin/sh
# meta-conditional.sh — acceptance gate for the BuildStream (?):
# per-arch conditional → project-B select() lowering.
#
# Drives the conditional-greet fixture through write-a:
#
#   1. cmd/write-a parses greet.bst, extracts the (?): branches
#      from variables: into structured form, and renders project A.
#      The element references %{arch-marker} (set per arch via the
#      (?): block) in its install-commands, so write-a emits
#      `cmd = select({...})` over @platforms//cpu:* with one branch
#      per supported arch.
#   2. The driver inspects the rendered BUILD and asserts the
#      select() shape: x86_64 / aarch64 / ppc64le get their per-arch
#      paths (matching the (?): branches), and the other supported
#      arches fall through to the `unknown` default.
#
# No bazel-build phase: select() resolution requires a target
# platform binding to choose a branch, which we'd need to set via
# --platforms=@platforms//... — out of scope for the v1 render-
# phase gate.

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

fixture="testdata/meta-project/conditional-greet"

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
        echo "meta-conditional: missing rendered project A file $f" >&2
        exit 1
    fi
done
build_path="$A/elements/greet/BUILD.bazel"
# select() shape.
for marker in 'cmd = select({' \
              '"@platforms//cpu:x86_64":' \
              '"@platforms//cpu:aarch64":' \
              '"@platforms//cpu:ppc64le":' \
              '"@platforms//cpu:x86_32":' \
              '"@platforms//cpu:riscv64":' \
              '"@platforms//cpu:loongarch64":'; do
    if ! grep -qF -- "$marker" "$build_path"; then
        echo "meta-conditional: project A BUILD missing marker: $marker" >&2
        cat "$build_path" >&2
        exit 1
    fi
done
# Per-arch resolved paths flow through (project.conf sets prefix=/usr).
for marker in 'install -D greeting.txt $$INSTALL_ROOT/usr/share/greetings/x86_64.txt' \
              'install -D greeting.txt $$INSTALL_ROOT/usr/share/greetings/aarch64.txt' \
              'install -D greeting.txt $$INSTALL_ROOT/usr/share/greetings/ppc.txt' \
              'install -D greeting.txt $$INSTALL_ROOT/usr/share/greetings/unknown.txt'; do
    if ! grep -qF -- "$marker" "$build_path"; then
        echo "meta-conditional: project A BUILD missing arch-specific install line:" >&2
        echo "  $marker" >&2
        cat "$build_path" >&2
        exit 1
    fi
done
echo "meta-conditional: render OK (cmd = select() over @platforms//cpu:*; per-arch resolved paths inlined)"

echo "meta-conditional: ok ((?): conditional lowered to project-B select(); arch-specific variable overrides resolve through Bazel rather than at write-a time)"
