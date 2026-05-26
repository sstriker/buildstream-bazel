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
# Note on the fixture's set(... CACHE STRING ...) blocks:
# cmake's standard initialization pre-populates the cache with
# empty entries for every CMAKE_<LANG>_FLAGS_<CONFIG> in
# CMAKE_CONFIGURATION_TYPES BEFORE any set() in CMakeLists.txt
# runs, so the example's defaults can't override on the first
# configure. Real-world operator flow is to pass -D on the cmake
# command line OR to set CMAKE_<LANG>_FLAGS_<CONFIG>_INIT in a
# toolchain file. This gate mirrors the -D path.

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

# Toolchain file to push the sanitizer flag defaults into
# CMAKE_<LANG>_FLAGS_<CONFIG>_INIT BEFORE cmake's standard cache
# initialization runs — the only way to land the values without a
# direct -D from the cmake command line. The fixture's
# CMakeLists.txt set(... CACHE STRING ...) blocks are no-ops on
# the first configure because cmake has already created empty
# cache entries by the time the set() runs.
toolchain="$stage/sanitizer.toolchain.cmake"
cat >"$toolchain" <<'EOF'
set(CMAKE_C_FLAGS_ASAN_INIT     "-fsanitize=address -fno-omit-frame-pointer -g -O1")
set(CMAKE_CXX_FLAGS_ASAN_INIT   "-fsanitize=address -fno-omit-frame-pointer -g -O1")
set(CMAKE_C_FLAGS_TSAN_INIT     "-fsanitize=thread -g -O1")
set(CMAKE_CXX_FLAGS_TSAN_INIT   "-fsanitize=thread -g -O1")
set(CMAKE_C_FLAGS_UBSAN_INIT    "-fsanitize=undefined -fno-omit-frame-pointer -g -O1")
set(CMAKE_CXX_FLAGS_UBSAN_INIT  "-fsanitize=undefined -fno-omit-frame-pointer -g -O1")
EOF

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
    --toolchain-cmake-file "$toolchain" \
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
