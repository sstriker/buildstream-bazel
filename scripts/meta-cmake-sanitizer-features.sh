#!/bin/sh
# meta-cmake-sanitizer-features.sh — render gate for Phase 5's
# multi-config + sanitizer-as-feature pipeline.
#
# Drives convert-element-cmake against the
# examples/sanitizer-features/cmake-side fixture with
# --build-types=Release,ASan,TSan,UBSan and
# --out-sanitizer-features=PATH. Asserts the generated features.bzl
# carries a usable feature("asan") definition with the
# -fsanitize=address flag the operator's toolchain wires up.
#
# Closes round-4's gap: Phase 5 was described as "operator runs
# convert-element-cmake with --out-sanitizer-features=..." but
# no automated test verified the end-to-end shape worked.
#
# The example fixture uses the CMAKE_<LANG>_FLAGS_<CONFIG>_INIT
# pattern (seeded before project()) so cmake's standard cache-init
# path picks up the defaults — no -D or toolchain-file workaround
# needed here. The gate runs convert-element-cmake straight against
# the fixture and asserts on the emitted features.bzl shape.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v ninja >/dev/null 2>&1; then
    echo "skip: ninja not on PATH (multi-config needs the Ninja generator)"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

# Stage the example with minimal sources so cmake's
# add_library / add_executable resolve.
stage="$(mktemp -d)/cmake-side"
trap "rm -rf '$(dirname "$stage")'" EXIT
mkdir -p "$stage/src" "$stage/include"
cp "$repo_root/examples/sanitizer-features/cmake-side/CMakeLists.txt" "$stage/"
echo 'int lib(void){return 1;}' >"$stage/src/lib.c"
echo 'int main(void){return 0;}' >"$stage/src/main.c"

out_features="$stage/features.bzl"
out_build="$stage/BUILD.bazel.out"

# --probe-genex=false: probe-genex emits per-target file(GENERATE)
# outputs that multi-config cmake errors on ("Evaluation file to be
# written multiple times with different content"). Disabling
# probe-genex here is the narrow workaround; the genex-probe +
# multi-config interaction is a separate known issue tracked
# outside this gate.
#
# The convert may exit non-zero on downstream stages (ninja-parse
# of multi-config layouts, ToIR's per-config splits) — those are
# orthogonal to the sanitizer-feature emit which runs early
# (right after fileapi.Load, well before ninja). We assert on the
# features.bzl shape independently of the overall exit code so
# the gate stays focused on the Phase-5 emit contract.
"$bin_dir/convert-element-cmake" \
    --source-root "$stage" \
    --build-types Release,ASan,TSan,UBSan \
    --probe-genex=false \
    --out-build "$out_build" \
    --out-sanitizer-features "$out_features" \
    >"$stage/convert.stdout" 2>"$stage/convert.stderr" || true

if [ ! -f "$out_features" ]; then
    echo "FAIL: features.bzl not written by Phase-5 emit"
    echo "   (check stderr below — the feature-write step runs"
    echo "   immediately after fileapi.Load; failure to write"
    echo "   here means the emit stage itself failed, not a"
    echo "   downstream ninja-parse issue)"
    sed 's/^/   stderr: /' "$stage/convert.stderr"
    exit 1
fi

# Verify the asan feature definition is present and carries the
# -fsanitize=address flag.
if ! grep -q 'name = "asan"' "$out_features"; then
    echo "FAIL: features.bzl missing feature(\"asan\")"
    sed 's/^/   /' "$out_features"
    exit 1
fi
if ! grep -q '\-fsanitize=address' "$out_features"; then
    echo "FAIL: features.bzl asan feature missing -fsanitize=address flag"
    sed 's/^/   /' "$out_features"
    exit 1
fi

echo "ok  meta-cmake-sanitizer-features: multi-config + sanitizerfeatures.Emit produced usable feature(\"asan\")"
