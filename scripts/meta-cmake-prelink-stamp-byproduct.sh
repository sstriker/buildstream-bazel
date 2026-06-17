#!/bin/sh
# meta-cmake-prelink-stamp-byproduct.sh — render+build gate for the TARGET-event
# add_custom_command "stamp" pattern (PRE_BUILD / PRE_LINK / POST_BUILD).
#
# add_custom_command(TARGET producer PRE_LINK COMMAND … BYPRODUCTS gen_impl.c)
# attaches a build-event hook that GENERATES a file. cmake folds the command onto
# producer's link edge and lists gen_impl.c as an extra output of that LINKER
# edge — so recoverGenrule can't reach it (the producing rule isn't a
# CUSTOM_COMMAND) and the byproduct would dangle for any consumer. The converter
# must instead synthesize a genrule producing the byproduct (lowerTargetEventCommands)
# and register it so the consuming target (which compiles gen_impl.c) resolves.
#
# Asserts (rendered BUILD):
#   1. A genrule (cmake-codegen-target-event-command) produces gen_impl.c.
#   2. The consumer target's srcs reference gen_impl.c (resolved, not dropped).
# Bazel-build half (bazel >= 7): //:consumer builds — proving the byproduct is
# produced by the synthesized genrule and compiled into the consumer.
#
# Gating: skips cleanly when cmake isn't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/prelink-stamp-byproduct"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --cmake-define BUILD_SHARED_LIBS=OFF \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- generated BUILD.bazel ---"
    sed 's/^/   /' "$out_build"
    exit 1
}

attr_block() { awk -v pat="$1" '$0 ~ pat {f=1} f {print} /\]/ {if(f)f=0}' "$out_build"; }

# 1. A target-event genrule produces gen_impl.c.
grep -qF 'cmake-codegen-target-event-command' "$out_build" \
    || fail "no cmake-codegen-target-event-command genrule emitted for the PRE_LINK byproduct"
grep -qE 'outs = \["gen_impl\.c"\]' "$out_build" \
    || fail "the PRE_LINK byproduct gen_impl.c is not a genrule output"

# 2. The consumer compiles the recovered byproduct (resolved, not dropped).
printf '%s\n' "$(attr_block '^    name = "consumer"')" | grep -qF '"gen_impl.c"' \
    || fail "consumer srcs do not reference the recovered byproduct gen_impl.c"

echo "ok  meta-cmake-prelink-stamp-byproduct: PRE_LINK BYPRODUCTS recovered as a genrule + consumer resolves it"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-prelink-stamp-byproduct: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-prelink-stamp-byproduct: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp -r "$fixture/." "$ws/"
cp "$out_build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "prelinkstamp", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:consumer) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:consumer failed (the PRE_LINK byproduct genrule didn't produce gen_impl.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-prelink-stamp-byproduct: //:consumer builds from the genrule-produced PRE_LINK byproduct"
