#!/bin/sh
# meta-cmake-shared-library.sh — render+build gate for faithful SHARED conversion
# (--emit-shared-libraries), focused on the run/test contract: the produced .so
# must be carried in runfiles so `bazel run` / `bazel test` LOAD it at runtime.
#
# The fixture has a SHARED `foo` consumed two ways: directly (direct_app) and
# transitively through a STATIC `mid` that PUBLIC-links foo (app / app_test). The
# converter emits foo as a cc_library impl + a sibling cc_shared_library, and
# wires each linking consumer's dynamic_deps to `:foo_shared` (the transitive case
# included, since cmake flattens the PUBLIC link interface). The payoff this gate
# guards: bazel build, then RUN the binaries and RUN the test — each loads
# libfoo.so from runfiles and exits 0 only when foo()/mid() return the right value.
#
# This is the SHARED feature's first lightweight render gate (it was previously
# only exercised by the heavy survey). Gating: skips cleanly when cmake isn't on
# PATH; the build/run half additionally needs bazel >= 7.

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

fixture="$repo_root/converter/testdata/sample-projects/shared-library"
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

attr_block() { awk -v pat="$1" '$0 ~ pat {f=1} f {print} /^\)/ {if(f)f=0}' "$build"; }

# 1. The SHARED lib renders a real cc_shared_library wrapper over its impl.
grep -qF 'cc_shared_library(' "$build" || fail "no cc_shared_library emitted for the SHARED target"
grep -qF 'name = "foo_shared"' "$build" || fail "foo_shared wrapper missing"
# 2. Each linking consumer dynamic-deps the .so wrapper — direct AND transitive.
printf '%s\n' "$(attr_block '^    name = "direct_app"')" | grep -qF 'dynamic_deps = [":foo_shared"]' \
    || fail "direct consumer not wired to the shared .so via dynamic_deps"
printf '%s\n' "$(attr_block '^    name = "app"')" | grep -qF 'dynamic_deps = [":foo_shared"]' \
    || fail "transitive consumer (app -> mid -> foo) not wired to the shared .so"
printf '%s\n' "$(attr_block '^    name = "app_test"')" | grep -qF 'dynamic_deps = [":foo_shared"]' \
    || fail "test consumer not wired to the shared .so"

echo "ok  meta-cmake-shared-library: SHARED target -> cc_shared_library + consumers' dynamic_deps wired (direct + transitive)"

# --- Bazel build + RUN + TEST half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-shared-library: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-shared-library: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/foo.c "$fixture"/mid.c "$fixture"/main.c "$fixture"/direct_main.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "sharedlib", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
run() {
    # shellcheck disable=SC2086
    (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} "$@" ${META_BAZEL_BUILD_ARGS:-}) >"$work_dir/bazel.log" 2>&1 || {
        echo "FAIL: bazel $* failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
    }
}
run build //...
# RUN both binaries: each loads libfoo.so from runfiles and exits 0.
run run //:direct_app
run run //:app
# TEST: the .so loads under `bazel test`'s runfiles too.
run test --test_output=errors //:app_test

echo "ok  meta-cmake-shared-library: //:app, //:direct_app run + //:app_test passes — libfoo.so loaded from runfiles"
