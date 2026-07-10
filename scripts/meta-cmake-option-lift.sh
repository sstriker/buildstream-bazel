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
# Drives convert-element-cmake --lift-options
# FOO_FEATURE,BACKEND,BUILD_EXTRA_TOOL against
# converter/testdata/sample-projects/option-lift (a BOOL option gating
# a private define + an extra source; an enum STRING+STRINGS option
# gating a define + a configure_file body; a BOOL option gating a
# whole cc_binary, rendered via a target_compatible_with select over
# @platforms//:incompatible).
# Asserts (rendered BUILD): the BOOL define and source ride the
# //options:foo_feature_on arm and are GONE from the flat baseline
# (the off arm must not compile them); the enum define rides the
# //options:backend_fast arm; the configure_file body renders as a
# write_file content select() with the fast arm's body and the
# configured value as //conditions:default; and the //options package
# carries the bool_flag + the string_flag (with values) and their
# config_settings. Bazel-build half (bazel >= 7): builds the library
# under both bool values (feature object flips in/out of the archive)
# and both enum values (the generated header's BACKEND_NAME flips).

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
    --lift-options FOO_FEATURE,BACKEND,BUILD_EXTRA_TOOL \
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
if grep -qE 'local_defines = \[[^]]*FOO_FEATURE' "$build"; then
    fail "FOO_FEATURE=1 still in flat local_defines (off arm would carry it)"
fi
grep -qF 'cmake options lifted to build-time flags' "$build" || fail "lifted-options header block missing"
grep -qF ' - BACKEND (//options:backend' "$build" || fail "BACKEND missing from the lifted-options header block"
grep -qF 'name = "foo_feature"' "$options_build" || fail "//options bool_flag missing"
grep -qF 'build_setting_default = True' "$options_build" || fail "bool_flag default should be True (configured ON)"
grep -qF 'name = "foo_feature_on"' "$options_build" || fail "config_setting foo_feature_on missing"
grep -qF 'name = "foo_feature_off"' "$options_build" || fail "config_setting foo_feature_off missing"

# Enum (STRING + STRINGS) option: string_flag + per-value settings,
# defines arm, and the configure_file body as a content select().
grep -qF '"//options:backend_fast": ["USE_FAST_BACKEND=1"]' "$build" || fail "enum defines select arm missing USE_FAST_BACKEND=1"
# Multi-axis composition: foo's defines carry arms from TWO options
# (BACKEND + FOO_FEATURE) — one shared select() would be an
# "Illegal ambiguous match" whenever both flags' conditions hold, so
# the emitter must render one select() per flag family, concatenated.
foo_stanza=$(sed -n '/name = "foo"/,/^)/p' "$build")
printf '%s' "$foo_stanza" | grep -qF '}) + select({' || fail "defines arms from two options share one select() (ambiguous-match shape)"
grep -qF 'name = "backend"' "$options_build" || fail "//options string_flag missing"
grep -qF 'build_setting_default = "ref"' "$options_build" || fail "string_flag default should be ref (configured value)"
grep -qF '"ref",' "$options_build" || fail "string_flag values missing ref"
grep -qF '"fast",' "$options_build" || fail "string_flag values missing fast"
grep -qF 'name = "backend_ref"' "$options_build" || fail "config_setting backend_ref missing"
grep -qF 'name = "backend_fast"' "$options_build" || fail "config_setting backend_fast missing"
grep -qF 'content = select({' "$build" || fail "write_file content is not a select (per-option bake missing)"
grep -qF '"#define BACKEND_NAME \"fast\""' "$build" || fail "fast arm body missing from content select"
grep -qF '"#define BACKEND_NAME \"ref\""' "$build" || fail "ref (default) arm body missing from content select"
grep -qF 'cmake-codegen-per-option-content' "$build" || fail "per-option content audit tag missing"
# The template embeds CMAKE_CURRENT_BINARY_DIR: flip scratch-dir
# spellings must canonicalize onto the primary build dir (and strip),
# not leak as throwaway paths or fabricate spelling-only select arms.
if grep -qE -- '-opt-[0-9]+-' "$build"; then
    fail "flip scratch-dir path leaked into the rendered BUILD"
fi
grep -qE 'lift-options FOO_FEATURE: lifted .* 0 write_file body' "$work_dir/convert.stderr" || fail "FOO_FEATURE gained a content select — its flip differs only by scratch-dir spelling (canonicalization regression)"

# Target-existence gating: BUILD_EXTRA_TOOL gates a whole cc_binary;
# the target renders unconditionally with a target_compatible_with
# select() marking it incompatible under the OFF arm.
grep -qF 'name = "extra_tool"' "$build" || fail "option-gated cc_binary extra_tool missing from the BUILD"
# Scope the gate assertions to the extra_tool stanza itself, not the
# whole BUILD (another rule matching would false-positive).
tool_stanza=$(sed -n '/name = "extra_tool"/,/^)/p' "$build")
printf '%s' "$tool_stanza" | grep -qF 'target_compatible_with = select({' || fail "extra_tool lacks the target_compatible_with select (existence gate)"
printf '%s' "$tool_stanza" | grep -qF '"//options:build_extra_tool_off": ["@platforms//:incompatible"]' || fail "extra_tool lacks the incompatible arm for build_extra_tool_off"
grep -qF 'name = "build_extra_tool"' "$options_build" || fail "//options bool_flag for BUILD_EXTRA_TOOL missing"

echo "ok  meta-cmake-option-lift: bool + enum option arms, flag package, and per-option content select rendered"

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
cp "$fixture"/common.c "$fixture"/feature.c "$fixture"/tool.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cp "$options_build" "$ws/options/BUILD.bazel"
# The write_file rule provides cfg.h; the source-tree template isn't
# needed in the test workspace.
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

check_backend() {
    value="$1"; want="$2"
    # shellcheck disable=SC2086
    if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
            build ${META_BAZEL_BUILD_ARGS:-} --//options:backend="$value" //:gen_cfg_h) >"$work_dir/bazel-backend-$value.log" 2>&1; then
        echo "FAIL: bazel build under --//options:backend=$value failed"
        sed 's/^/   /' "$work_dir/bazel-backend-$value.log"
        exit 1
    fi
    got=$(grep -h "BACKEND_NAME" "$ws/bazel-bin/cfg.h" 2>/dev/null) || {
        echo "FAIL: --//options:backend=$value: bazel-bin/cfg.h missing or lacks BACKEND_NAME"
        ls "$ws/bazel-bin" 2>/dev/null | sed 's/^/   /'
        exit 1
    }
    case "$got" in
        *"\"$want\""*) ;;
        *) echo "FAIL: --//options:backend=$value: generated header carries '$got', want \"$want\""; exit 1 ;;
    esac
}
check_backend ref ref
check_backend fast fast

# Existence gate: a //... build under the gating value SKIPS the
# incompatible cc_binary (no analysis error, no binary produced);
# under the default it builds.
check_tool() {
    value="$1"; want_binary="$2"
    # shellcheck disable=SC2086
    if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
            build ${META_BAZEL_BUILD_ARGS:-} --//options:build_extra_tool="$value" //...) >"$work_dir/bazel-tool-$value.log" 2>&1; then
        echo "FAIL: bazel build //... under --//options:build_extra_tool=$value failed"
        sed 's/^/   /' "$work_dir/bazel-tool-$value.log"
        exit 1
    fi
    if [ -e "$ws/bazel-bin/extra_tool" ]; then got=1; else got=0; fi
    if [ "$got" != "$want_binary" ]; then
        echo "FAIL: --//options:build_extra_tool=$value: extra_tool built=$got, want $want_binary"
        exit 1
    fi
}
check_tool true 1
check_tool false 0

echo "ok  meta-cmake-option-lift: feature object, generated header, and gated tool flip per --//options flags (no cmake)"
