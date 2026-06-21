#!/bin/sh
# meta-cmake-shared-soversion.sh — ELF-fidelity gate for faithful SHARED
# conversion (--emit-shared-libraries): the produced .so must embed the SAME
# SONAME cmake does. cmake sets SOVERSION 1 on a versioned SHARED lib, so its
# libgreet.so.1.2.3 carries soname `libgreet.so.1`; the bazel cc_shared_library
# defaults to NO soname unless threaded a -Wl,-soname user_link_flag. This gate
# converts, asserts the soname linkopt renders, then builds and reads the
# SONAME out of the produced .so with readelf and compares it to cmake's own
# reference build — proving byte-for-byte soname parity, not just that some
# soname is present.
#
# Gating: skips cleanly without cmake / readelf; the build half needs bazel >= 7.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v readelf >/dev/null 2>&1; then
    echo "skip: readelf not on PATH"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/shared-soversion"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --emit-shared-libraries \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

soname_of() { readelf -d "$1" 2>/dev/null | awk '/SONAME/{print $NF}' | tr -d '[]'; }
# VERDEFNUM (count of .gnu.version_d nodes) + the sorted version-node names —
# the byte-level signature of the symbol version script.
verdefnum_of() { readelf -d "$1" 2>/dev/null | awk '/VERDEFNUM/{print $NF}'; }
# Names from the version DEFINITIONS section (.gnu.version_d) only — the
# version-script's output. Excludes the version NEEDS section (.gnu.version_r,
# the libc GLIBC_* requirements), which is a libc-linking artifact, not part of
# the version-script fidelity being compared.
vernames_of() {
    readelf --version-info "$1" 2>/dev/null | awk '
        /\.gnu\.version_d/ {d=1; next}
        /\.gnu\.version_r/ {d=0}
        d && /Name:/ {sub(/.*Name: /,""); sub(/[[:space:]].*/,""); print}' | sort
}

# 0. cmake reference: what soname + version nodes does cmake actually embed?
refb="$work_dir/refb"
cmake -S "$fixture" -B "$refb" -G Ninja >/dev/null 2>&1 || fail "cmake configure (reference) failed"
cmake --build "$refb" >/dev/null 2>&1 || fail "cmake build (reference) failed"
ref_so="$(find "$refb" -name 'libgreet.so.*' -type f | head -1)"
[ -n "$ref_so" ] || fail "cmake reference build produced no libgreet.so.*"
ref_soname="$(soname_of "$ref_so")"
[ -n "$ref_soname" ] || fail "cmake reference .so carries no SONAME (fixture/toolchain issue)"
ref_verdefnum="$(verdefnum_of "$ref_so")"
ref_vernames="$(vernames_of "$ref_so")"
[ "${ref_verdefnum:-0}" -ge 2 ] || fail "cmake reference .so has no version-script VERDEF (fixture/toolchain issue)"

# 1. The cc_shared_library threads the soname AND the version-script (the
#    latter as an unquoted $(location ...) with the map staged).
grep -qF "\"-Wl,-soname,$ref_soname\"" "$build" \
    || fail "cc_shared_library missing user_link_flags soname matching cmake's '$ref_soname'"
grep -qF '"-Wl,--version-script,$(location greet.map)"' "$build" \
    || fail "cc_shared_library missing version-script user_link_flag (unquoted \$(location))"
grep -qF 'additional_linker_inputs = ["greet.map"]' "$build" \
    || fail "cc_shared_library missing additional_linker_inputs = [\"greet.map\"]"
# The impl cc_library must NOT carry the version-script (it would propagate to
# every consumer's link).
awk '/^    name = "greet"/{f=1} f{print} /^\)/{if(f)exit}' "$build" | grep -qF 'version-script' \
    && fail "impl cc_library leaks the version-script (propagates to consumers)"

echo "ok  meta-cmake-shared-soversion: cc_shared_library threads -Wl,-soname,$ref_soname + version-script \$(location greet.map) (impl clean)"

# --- Bazel build + readelf parity half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-shared-soversion: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-shared-soversion: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/greet.c "$fixture"/main.c "$fixture"/greet.map "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sharedsoversion", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
bz() {
    # shellcheck disable=SC2086
    (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} "$@" ${META_BAZEL_BUILD_ARGS:-}) >"$work_dir/bazel.log" 2>&1 || {
        echo "FAIL: bazel $* failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
    }
}
bz build //...
bz run //:app

bz_so="$(cd "$ws" && find -L bazel-bin -name "$ref_soname" ! -name '*.params' -type f | head -1)"
[ -n "$bz_so" ] || fail "bazel build produced no $ref_soname"
bz_soname="$(soname_of "$ws/$bz_so")"
[ "$bz_soname" = "$ref_soname" ] \
    || fail "soname mismatch: bazel .so '$bz_soname' != cmake '$ref_soname'"

# Version-script parity: same VERDEF node count AND same version names as cmake.
bz_verdefnum="$(verdefnum_of "$ws/$bz_so")"
bz_vernames="$(vernames_of "$ws/$bz_so")"
[ "$bz_verdefnum" = "$ref_verdefnum" ] \
    || fail "VERDEFNUM mismatch: bazel .so '$bz_verdefnum' != cmake '$ref_verdefnum' (version script not applied)"
[ "$bz_vernames" = "$ref_vernames" ] \
    || fail "version-node names mismatch:
   bazel: $(printf '%s' "$bz_vernames" | tr '\n' ' ')
   cmake: $(printf '%s' "$ref_vernames" | tr '\n' ' ')"

echo "ok  meta-cmake-shared-soversion: bazel .so SONAME '$bz_soname' + VERDEF ($bz_verdefnum nodes: $(printf '%s' "$bz_vernames" | tr '\n' ' ')) == cmake's; //:app links + runs"
