#!/bin/sh
# meta-cmake-option-lift.sh — render+build gate for the cmake option()
# lift (--lift-options; stages a+b of the option-lift ROADMAP.md item).
#
# cmake's option() is configure-time-resolved, so by default the
# converter bakes the configured value in and only records the
# inventory as a header comment. --lift-options NAME runs one extra
# cold configure with NAME flipped, folds the attribute deltas between
# the two codemodels into select() arms over //options:<name>_{on,off}
# config_settings, and emits the backing //options package
# (bool_flag + config_setting pair) via --out-option-settings.
#
# Drives convert-element-cmake --lift-options FOO_FEATURE against
# converter/testdata/sample-projects/option-lift (an option gating a
# private define + an extra source). Asserts (rendered BUILD): the
# define and the source ride the //options:foo_feature_on arm and are
# GONE from the flat baseline (the off arm must not compile them), and
# the //options package carries the bool_flag with the configured
# default. Bazel-build half (bazel >= 7): builds the library under both
# flag values and asserts the feature object flips in and out of the
# archive.

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

fixture="$repo_root/converter/testdata/sample-projects/option-lift"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --lift-options FOO_FEATURE \
    --out-build "$work_dir/BUILD.bazel" \
    --out-option-settings "$work_dir/options-BUILD.bazel" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

build="$work_dir/BUILD.bazel"
options_build="$work_dir/options-BUILD.bazel"

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    echo "   --- options-BUILD.bazel ---"
    sed 's/^/   /' "$options_build" 2>/dev/null || true
    exit 1
}

grep -qF '"//options:foo_feature_on": ["feature.c"]' "$build" || fail "srcs select arm missing feature.c"
grep -qF '"//options:foo_feature_on": ["FOO_FEATURE=1"]' "$build" || fail "defines select arm missing FOO_FEATURE=1"
grep -qF 'srcs = ["common.c"] + select({' "$build" || fail "feature.c not deduped out of the flat srcs baseline"
grep -qF 'FOO_FEATURE=1' "$build" | head -1 >/dev/null
if grep -qE 'local_defines = \[[^]]*FOO_FEATURE' "$build"; then
    fail "FOO_FEATURE=1 still in flat local_defines (off arm would carry it)"
fi
grep -qF 'cmake options lifted to build-time flags' "$build" || fail "lifted-options header block missing"
grep -qF 'name = "foo_feature"' "$options_build" || fail "//options bool_flag missing"
grep -qF 'build_setting_default = True' "$options_build" || fail "bool_flag default should be True (configured ON)"
grep -qF 'name = "foo_feature_on"' "$options_build" || fail "config_setting foo_feature_on missing"
grep -qF 'name = "foo_feature_off"' "$options_build" || fail "config_setting foo_feature_off missing"

echo "ok  meta-cmake-option-lift: option-gated define + source rendered as //options select() arms, flag package emitted"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-option-lift: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-option-lift: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/options"
cp "$fixture"/common.c "$fixture"/feature.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cp "$options_build" "$ws/options/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "optionlift", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
check_arm() {
    value="$1"; want_feature="$2"
    # shellcheck disable=SC2086
    if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
            build ${META_BAZEL_BUILD_ARGS:-} --//options:foo_feature="$value" //:foo) >"$work_dir/bazel-$value.log" 2>&1; then
        echo "FAIL: bazel build under --//options:foo_feature=$value failed"
        sed 's/^/   /' "$work_dir/bazel-$value.log"
        exit 1
    fi
    archive=$(find "$ws/bazel-bin" -name 'libfoo.a' | head -1)
    [ -n "$archive" ] || { echo "FAIL: libfoo.a not found under bazel-bin"; exit 1; }
    if ar t "$archive" | grep -q 'feature'; then
        got=1
    else
        got=0
    fi
    if [ "$got" != "$want_feature" ]; then
        echo "FAIL: --//options:foo_feature=$value: feature.o in archive = $got, want $want_feature"
        ar t "$archive" | sed 's/^/   /'
        exit 1
    fi
}
check_arm true 1
check_arm false 0

echo "ok  meta-cmake-option-lift: feature object flips in/out of the archive per --//options:foo_feature (no cmake)"
