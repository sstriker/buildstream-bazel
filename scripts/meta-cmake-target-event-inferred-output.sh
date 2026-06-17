#!/bin/sh
# meta-cmake-target-event-inferred-output.sh — render+build gate for inferring a
# TARGET-event command's output from the command line when it declares NO
# BYPRODUCTS.
#
# add_custom_command(TARGET producer PRE_LINK COMMAND cat in.c.in > out.c) names
# its output only by the `>` redirect — no BYPRODUCTS. cmake folds the command
# onto producer's link edge (not a CUSTOM_COMMAND, so recoverGenrule can't reach
# it); with no BYPRODUCTS the command would be dropped as a pure side-effect.
# lowerTargetEventCommands instead INFERS the redirect target as an output
# (best-effort), synthesizes a genrule producing it, and registers it so the
# consumer (which compiles the generated source) resolves.
#
# Asserts (rendered BUILD):
#   1. A genrule tagged cmake-codegen-target-event-inferred-output produces gen_impl.c.
#   2. The genrule cmd preserves the redirect anchored to $(RULEDIR).
#   3. The consumer target's srcs reference gen_impl.c (resolved, not dropped).
# Bazel-build half (bazel >= 7): //:consumer builds — proving the inferred-output
# genrule's bash actually runs the redirect and produces the source.
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

fixture="$repo_root/converter/testdata/sample-projects/target-event-inferred-output"
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

# 1. An inferred-output target-event genrule produces gen_impl.c.
grep -qF 'cmake-codegen-target-event-inferred-output' "$out_build" \
    || fail "no inferred-output genrule emitted for the no-BYPRODUCTS redirect command"
grep -qE 'outs = \["gen_impl\.c"\]' "$out_build" \
    || fail "the inferred output gen_impl.c is not a genrule output"

# 2. The redirect is preserved, anchored to $(RULEDIR).
grep -qF '> $(RULEDIR)/gen_impl.c' "$out_build" \
    || fail "the inferred-output genrule cmd does not preserve the redirect anchored to \$(RULEDIR)"

# 3. The consumer compiles the inferred output (resolved, not dropped).
printf '%s\n' "$(attr_block '^    name = "consumer"')" | grep -qF '"gen_impl.c"' \
    || fail "consumer srcs do not reference the inferred output gen_impl.c"

# A no-BYPRODUCTS, no-inferable-output command stays a dropped side-effect: the
# breadcrumb must announce the inference happened (best-effort, not authoritative).
grep -qF 'inferred output' "$work_dir/convert.stderr" \
    || fail "expected the best-effort inferred-output breadcrumb on stderr"

echo "ok  meta-cmake-target-event-inferred-output: no-BYPRODUCTS redirect output inferred as a genrule + consumer resolves it"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-target-event-inferred-output: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-target-event-inferred-output: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp -r "$fixture/." "$ws/"
cp "$out_build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "tgteventinfer", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:consumer) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:consumer failed (the inferred-output genrule didn't produce gen_impl.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cmake-target-event-inferred-output: //:consumer builds from the inferred-output genrule"
