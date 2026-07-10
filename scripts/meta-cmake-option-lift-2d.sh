#!/bin/sh
# meta-cmake-option-lift-2d.sh — render gate for the 2D option x config
# fold (--lift-options composed with --build-types).
#
# Under a multi-config configure a fact can vary on either axis or on
# both at once ($<$<AND:$<CONFIG:Debug>,$<BOOL:${FOO}>>:...>-shaped).
# Additive selects can't subtract, so pure //config:* arms are only
# honest for facts config-conditional under EVERY option value: the 2D
# fold classifies each fact over the (config x option-value) grid —
# pure-option facts land on //options arms, pure-config facts stay on
# the base fold's //config arms, and mixed-support facts move onto
# skylib config_setting_group AND-arms (emitted into the //options
# package) and are REMOVED from the base fold's plain //config arm,
# which would otherwise over-apply them under option values outside
# the support.
#
# Drives convert-element-cmake --build-types Debug,Release
# --lift-options FOO_FEATURE,BACKEND,BUILD_EXTRA_TOOL against
# converter/testdata/sample-projects/option-lift, whose fixture carries
# a $<$<AND:$<CONFIG:Debug>,$<BOOL:${FOO_FEATURE}>>:FOO_DEBUG_EXTRA=1>
# interaction define alongside the single-axis shapes.

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
    --build-types Debug,Release \
    --lift-options FOO_FEATURE,BACKEND,BUILD_EXTRA_TOOL \
    --out-build "$work_dir/BUILD.bazel" \
    --out-option-settings "$work_dir/options-BUILD.bazel" \
    --out-config-settings "$work_dir/config-BUILD.bazel" \
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

# Mixed-support fact: on the AND arm, and ONLY there.
grep -qF '"//options:debug_and_foo_feature_on": ["FOO_DEBUG_EXTRA=1"]' "$build" || fail "mixed fact missing from the AND arm"
if sed -n '/"\/\/config:debug": \[/,/\]/p' "$build" | grep -q "FOO_DEBUG_EXTRA"; then
    fail "mixed fact leaked into a plain //config:debug arm (over-applies under FOO_FEATURE=off)"
fi
# The AND group is emitted via skylib's selects.config_setting_group.
grep -qF 'load("@bazel_skylib//lib:selects.bzl", "selects")' "$options_build" || fail "selects.bzl load missing"
grep -qF 'name = "debug_and_foo_feature_on"' "$options_build" || fail "config_setting_group missing"
grep -qF '"//config:debug",' "$options_build" || fail "group match_all missing the config setting"
grep -qF '"//options:foo_feature_on",' "$options_build" || fail "group match_all missing the option setting"
# Single-axis shapes stay intact alongside the 2D fold.
grep -qF '"//options:backend_fast": ["USE_FAST_BACKEND=1"]' "$build" || fail "pure enum option arm missing"
grep -qF '"//options:foo_feature_on": ["FOO_FEATURE=1"]' "$build" || fail "pure bool option arm missing"
grep -qF 'name = "build_type"' "$work_dir/config-BUILD.bazel" || fail "//config package missing"

echo "ok  meta-cmake-option-lift-2d: option x config interaction rendered as a config_setting_group AND-arm; pure arms per axis"
