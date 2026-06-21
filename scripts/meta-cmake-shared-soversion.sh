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

# 0. cmake reference: what soname does cmake actually embed?
refb="$work_dir/refb"
cmake -S "$fixture" -B "$refb" -G Ninja >/dev/null 2>&1 || fail "cmake configure (reference) failed"
cmake --build "$refb" >/dev/null 2>&1 || fail "cmake build (reference) failed"
ref_so="$(find "$refb" -name 'libgreet.so.*' -type f | head -1)"
[ -n "$ref_so" ] || fail "cmake reference build produced no libgreet.so.*"
ref_soname="$(soname_of "$ref_so")"
[ -n "$ref_soname" ] || fail "cmake reference .so carries no SONAME (fixture/toolchain issue)"

# 1. The cc_shared_library threads the soname as a user_link_flag.
grep -qF "user_link_flags = [\"-Wl,-soname,$ref_soname\"]" "$build" \
    || fail "cc_shared_library missing user_link_flags soname matching cmake's '$ref_soname'"

echo "ok  meta-cmake-shared-soversion: cc_shared_library threads -Wl,-soname,$ref_soname (matches cmake)"

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
cp "$fixture"/greet.c "$fixture"/main.c "$ws/"
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

echo "ok  meta-cmake-shared-soversion: bazel .so SONAME '$bz_soname' == cmake's; //:app links + runs"
