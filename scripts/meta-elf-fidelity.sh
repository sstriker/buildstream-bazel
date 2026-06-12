#!/bin/sh
# meta-elf-fidelity.sh — self-contained gate for the ELF dynamic-section
# fidelity lens (cmd/elf-fidelity-compare).
#
# The ELF lens is the shared-library sibling of the symbol-fidelity lens: it
# compares the DYNAMIC-section / ABI surface of a cmake-built .so vs a
# Bazel-built .so — SONAME, DT_NEEDED, symbol versioning (.gnu.version_d), and
# DT_RPATH/DT_RUNPATH — facts the nm-based exported-symbol-SET compare can't
# express (notably symbol versioning: matching symbol names under different
# version tags is an ABI break nm passes clean).
#
# This gate doesn't need a full convert/survey: it builds two trivial shared
# objects directly with cc and runs the tool against them.
#   1. Two matching .so's (same soname + version node) → exit 0, no impactful.
#   2. A divergent "bazel" .so (host-leak RUNPATH + dropped version node) →
#      exit 1, with the report naming the impactful classes.
# Asserting both directions keeps the gate load-bearing: assertion 1 can't pass
# vacuously while the tool always exits 0.
#
# Gating: skips cleanly when cc / readelf / go are absent.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cc >/dev/null 2>&1 || { echo "skip: cc not on PATH"; exit 0; }
command -v readelf >/dev/null 2>&1 || { echo "skip: readelf not on PATH"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -C "$repo_root" -o "$bin_dir/elf-fidelity-compare" ./cmd/elf-fidelity-compare
tool="$bin_dir/elf-fidelity-compare"

echo 'int foo(void){return 7;} int bar(void){return 9;}' > "$work_dir/a.c"
printf 'LIBFOO_1.0 {\n  global: foo; bar;\n  local: *;\n};\n' > "$work_dir/v.map"

# cmake side: soname + version node, no rpath.
cc -shared -fPIC -o "$work_dir/cmake.so" "$work_dir/a.c" \
   -Wl,-soname,libfoo.so.1 -Wl,--version-script,"$work_dir/v.map"

# bazel side (good): identical ABI surface, different path.
cc -shared -fPIC -o "$work_dir/bazel_good.so" "$work_dir/a.c" \
   -Wl,-soname,libfoo.so.1 -Wl,--version-script,"$work_dir/v.map"

# bazel side (bad): drops the version node AND bakes a /tmp RUNPATH (hermeticity
# leak) — the two impactful classes the lens must catch.
cc -shared -fPIC -o "$work_dir/bazel_bad.so" "$work_dir/a.c" \
   -Wl,-soname,libfoo.so.1 \
   -Wl,--enable-new-dtags -Wl,-rpath,/tmp/bazel-out/leak

# --- Assertion 1: matching .so's compare clean (exit 0). ---
if ! "$tool" --cmake-artifact "$work_dir/cmake.so" --bazel-artifact "$work_dir/bazel_good.so" \
       --report "$work_dir/good.json" >"$work_dir/good.out" 2>&1; then
    echo "FAIL: matching shared objects reported impactful deltas"
    sed 's/^/   /' "$work_dir/good.out"
    exit 1
fi

# --- Assertion 2: divergent .so trips exit 1 with the expected classes. ---
if "$tool" --cmake-artifact "$work_dir/cmake.so" --bazel-artifact "$work_dir/bazel_bad.so" \
       --report "$work_dir/bad.json" >"$work_dir/bad.out" 2>&1; then
    echo "FAIL: divergent shared object (host-leak RUNPATH + dropped version node) should exit 1"
    sed 's/^/   /' "$work_dir/bad.out"
    exit 1
fi
for kind in version-node-only-in-cmake rpath-host-leak-in-bazel; do
    if ! grep -qF "$kind" "$work_dir/bad.json"; then
        echo "FAIL: expected impactful delta '$kind' in the report"
        sed 's/^/   /' "$work_dir/bad.json"
        exit 1
    fi
done

# --- Assertion 2b (load-bearing): allowlisting both makes it clean again. ---
printf 'LIBFOO_1.0\n' > "$work_dir/allow.txt"
# The rpath leak isn't allowlistable (hermeticity), so drop it and re-check that
# the version-node allowlist alone clears the version delta.
cc -shared -fPIC -o "$work_dir/bazel_noveronly.so" "$work_dir/a.c" -Wl,-soname,libfoo.so.1
if ! "$tool" --cmake-artifact "$work_dir/cmake.so" --bazel-artifact "$work_dir/bazel_noveronly.so" \
       --allowlist "$work_dir/allow.txt" >"$work_dir/allow.out" 2>&1; then
    echo "FAIL: allowlisting LIBFOO_1.0 should clear the version-node delta"
    sed 's/^/   /' "$work_dir/allow.out"
    exit 1
fi

echo "ok  meta-elf-fidelity: SONAME / DT_NEEDED / version-node / RPATH deltas classified; impactful cases exit 1, allowlist suppresses"
