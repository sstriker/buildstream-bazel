#!/bin/sh
# meta-cmake-per-config-bake.sh — render+build gate for the per-build-type
# configure_file bake (--per-config-bake).
#
# A multi-config cmake generator runs configure ONCE with no
# CMAKE_BUILD_TYPE, so a configure_file body a single-config-idiomatic
# project derives from CMAKE_BUILD_TYPE (LLVM's abi-breaking.h:
# LLVM_ENABLE_ABI_BREAKING_CHECKS follows assertions — on for Debug, off
# for Release) bakes ONE config's view for every //config:* arm. The
# per-config bake passes re-configure once per --build-types entry
# (single-config, sibling scratch dirs), and bodies that differ become
# `content = select({"//config:<name>": …})` arms on the write_file, with
# the multi-config view as //conditions:default.
#
# Drives convert-element-cmake --build-types Debug,Release against
# converter/testdata/sample-projects/per-config-bake (an ENABLE_CHECKS
# value derived from CMAKE_BUILD_TYPE). Asserts (rendered BUILD): the
# write_file content is a select() whose debug arm carries 1 and release
# arm carries 0, tagged cmake-codegen-per-config-content. Bazel-build half
# (bazel >= 7): builds the consumer under BOTH --//config:build_type arms
# and asserts the generated header's value flips.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/per-config-bake"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --build-types Debug,Release \
    --out-build "$work_dir/BUILD.bazel" \
    --out-config-settings "$work_dir/config-BUILD.bazel" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

build="$work_dir/BUILD.bazel"

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

grep -qF 'content = select({' "$build" || fail "write_file content is not a select (per-config bake missing)"
grep -qF '"#define ENABLE_CHECKS 1"' "$build" || fail "debug arm value (1) missing"
grep -qF '"#define ENABLE_CHECKS 0"' "$build" || fail "release/default arm value (0) missing"
grep -qF 'cmake-codegen-per-config-content' "$build" || fail "audit tag missing"

echo "ok  meta-cmake-per-config-bake: CMAKE_BUILD_TYPE-derived configure_file body rendered as per-config content select()"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-per-config-bake: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-per-config-bake: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/config"
cp "$fixture"/uses.c "$fixture"/cfg.h.in "$ws/"
cp "$build" "$ws/BUILD.bazel"
cp "$work_dir/config-BUILD.bazel" "$ws/config/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "percfgbake", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
check_arm() {
    arm="$1"; want="$2"
    # shellcheck disable=SC2086
    if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
            build ${META_BAZEL_BUILD_ARGS:-} --//config:build_type="$arm" //:uses) >"$work_dir/bazel-$arm.log" 2>&1; then
        echo "FAIL: bazel build under --//config:build_type=$arm failed"
        sed 's/^/   /' "$work_dir/bazel-$arm.log"
        exit 1
    fi
    got=$(grep -h "ENABLE_CHECKS" "$ws/bazel-bin/cfg.h")
    case "$got" in
        *"$want") ;;
        *) echo "FAIL: $arm arm generated header carries '$got', want value $want"; exit 1 ;;
    esac
}
check_arm debug 1
check_arm release 0

echo "ok  meta-cmake-per-config-bake: the generated header's value flips per --//config:build_type arm (no cmake)"
