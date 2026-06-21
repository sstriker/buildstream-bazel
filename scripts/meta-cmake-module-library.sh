#!/bin/sh
# meta-cmake-module-library.sh — render+build gate for faithful MODULE
# conversion (--emit-shared-libraries), focused on the dlopen-by-EXACT-name
# contract that distinguishes a MODULE from a SHARED library.
#
# A cmake MODULE (`add_library(plugin MODULE ...)`) is a plugin dlopen'd at
# runtime and NEVER linked by a consumer. cmake names it `libplugin.so` (no
# soversion), and the host loads it with dlopen("libplugin.so"). The SHARED
# path suffixes an unversioned name to `.so.1` to dodge a collision with its
# impl cc_library's implicit dynamic output — but a MODULE has no link
# consumer pulling that impl, so there's no collision, and the suffix would
# break dlopen-by-name. This gate asserts the MODULE keeps cmake's exact
# name and that a host can actually dlopen it by that name.
#
# Note the underivable half: the converter does NOT (and cannot) wire the
# module into the host's runfiles — cmake has no static record of which
# executable dlopens which module (it's a runtime dlopen string). So the
# run step makes the .so findable via LD_LIBRARY_PATH, exactly the operator
# step the conversion leaves to the human. The gate validates filename
# fidelity, not auto-runfiles-wiring.
#
# Gating: skips cleanly when cmake isn't on PATH; the build/run half
# additionally needs bazel >= 7.

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

fixture="$repo_root/converter/testdata/sample-projects/module-library"
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

# 1. The MODULE renders a cc_shared_library with cmake's EXACT name — no
#    `.so.1` soversion suffix (that would break dlopen("libplugin.so")).
grep -qF 'cc_shared_library(' "$build" || fail "no cc_shared_library emitted for the MODULE target"
grep -qF 'shared_lib_name = "libplugin.so"' "$build" \
    || fail "MODULE .so not named exactly libplugin.so (dlopen-by-name would break)"
grep -qF 'libplugin.so.1' "$build" && fail "MODULE .so carries a soversion suffix — dlopen-by-name would break"
# 2. The host does NOT link the module (cmake has no static dlopen edge), so
#    no dynamic_deps wiring to the module — the conversion leaves runfiles
#    wiring to the operator.
attr_block() { awk -v pat="$1" '$0 ~ pat {f=1} f {print} /^\)/ {if(f)f=0}' "$build"; }
printf '%s\n' "$(attr_block '^    name = "host"')" | grep -qF 'plugin' \
    && fail "host should not reference the dlopen'd module (no static link edge in cmake)"
# 3. No convert-time absolute path leaks.
grep -qF '/tmp/' "$build" && fail "convert-time absolute path leaked into the BUILD file"

echo "ok  meta-cmake-module-library: MODULE -> cc_shared_library keeps exact libplugin.so; host carries no link edge"

# --- Bazel build + dlopen-by-name half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-module-library: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-module-library: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/plugin.c "$fixture"/host.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "modulelib", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
bz() {
    # shellcheck disable=SC2086
    (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} "$@" ${META_BAZEL_BUILD_ARGS:-}) >"$work_dir/bazel.log" 2>&1 || {
        echo "FAIL: bazel $* failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
    }
}
# The cc_shared_library + impl cc_library coexist (no collision: nothing links
# the module impl dynamically).
bz build //...

# dlopen-by-name: locate the produced .so and make it findable on the loader
# search path (the operator's runfiles step, which cmake metadata can't
# auto-derive), then run the host. It exits 0 only if dlopen("libplugin.so")
# resolved — proving the emitted filename is dlopen-compatible.
so="$(cd "$ws" && find -L bazel-bin -name 'libplugin.so' 2>/dev/null | head -1)"
[ -n "$so" ] || fail "built .so named libplugin.so not found under bazel-bin"
host_bin="$ws/$(cd "$ws" && find -L bazel-bin -name host -type f 2>/dev/null | head -1)"
[ -x "$host_bin" ] || fail "host binary not found under bazel-bin"
if LD_LIBRARY_PATH="$ws/$(dirname "$so")" "$host_bin"; then
    :
else
    echo "FAIL: host could not dlopen(\"libplugin.so\") by exact name (exit $?)"; exit 1
fi

echo "ok  meta-cmake-module-library: //... builds + host dlopen(\"libplugin.so\") resolves by exact cmake name"
