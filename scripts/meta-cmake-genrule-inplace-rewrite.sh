#!/bin/sh
# meta-cmake-genrule-inplace-rewrite.sh — render gate for the
# source-tree-input == build-tree-output genrule "in-place rewrite" fix
# (the LLVM Remarks.exports shape; docs/design/genrule-inplace-rewrite.md).
#
# A custom command that reads a file from the SOURCE tree and writes the
# SAME relative path into the BUILD tree used to produce a genrule with
# that path as BOTH an srcs entry and an outs entry — which Bazel rejects
# ("file X as both an input and an output") — and a cmd that anchored the
# input to $(RULEDIR) too, copying the output onto itself. The fix renames
# the colliding output (version.txt -> version.txt.gen) and disambiguates
# the cmd so the input reads the source and the output writes $(RULEDIR).
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/genrule-inplace-rewrite (a cc_library
# + an add_custom_command copying version.txt source -> same relative
# build path).
#
# Asserts on the emitted genrule:
#   1. outs is the RENAMED path (version.txt.gen), NOT the colliding
#      version.txt.
#   2. srcs still carries the source version.txt (the input read).
#   3. cmd reads the source input (bare version.txt) and writes
#      $(RULEDIR)/version.txt.gen — NOT $(RULEDIR)/version.txt for the
#      input (the old self-copy bug).
#   4. the cmake-codegen-genrule-inplace-rewrite audit tag is present.
#
# The bazel-build half (bazel >= 9) proves the load-bearing claim: the
# rule that previously failed at load ("input and output") now builds and
# produces the renamed output.

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

fixture="$repo_root/converter/testdata/sample-projects/genrule-inplace-rewrite"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

# Slice the genrule block (the fix renames the rule after its renamed
# first output -> custom_command_version_txt_gen).
blk="$(awk '
    /^genrule\(/ { in_blk = 1 }
    in_blk { print }
    in_blk && /^\)/ { exit }
' "$out_build")"

if [ -z "$blk" ]; then
    echo "FAIL: no genrule emitted in BUILD.bazel"
    sed 's/^/   /' "$out_build"
    exit 1
fi

fail() {
    echo "FAIL: $1"
    echo "--- genrule block ---"
    printf '%s\n' "$blk" | sed 's/^/   /'
    exit 1
}

# 1. outs is the renamed path, not the colliding source name.
printf '%s\n' "$blk" | grep -qF 'outs = ["version.txt.gen"]' \
    || fail "genrule outs should be the renamed [\"version.txt.gen\"] (no source-shadowing collision)"
printf '%s\n' "$blk" | grep -qF 'outs = ["version.txt"]' \
    && fail "genrule still declares outs = [\"version.txt\"] — collides with the source file"

# 2. srcs carries the source input.
printf '%s\n' "$blk" | grep -qF 'srcs = ["version.txt"]' \
    || fail "genrule srcs should carry the source version.txt input"

# 3. cmd reads the source input (bare version.txt) and writes the renamed
# output under $(RULEDIR); the input must NOT be anchored to $(RULEDIR).
# The double-suffix and self-copy shapes are asserted explicitly: the
# original assertions passed for years while the cmd was
# `cp $(RULEDIR)/version.txt.gen $(RULEDIR)/version.txt.gen` (the input
# anchored+renamed in lockstep with the output), because the trailing-
# space grep can't see a `.gen`-suffixed self-copy.
printf '%s\n' "$blk" | grep -qF '$(RULEDIR)/version.txt.gen' \
    || fail "cmd should write the renamed output to \$(RULEDIR)/version.txt.gen"
printf '%s\n' "$blk" | grep -qF '$(RULEDIR)/version.txt ' \
    && fail "cmd anchored the INPUT to \$(RULEDIR)/version.txt — the self-copy bug (input should read the source, not RULEDIR)"
printf '%s\n' "$blk" | grep -qF 'version.txt.gen.gen' \
    && fail "output renamed twice (stage-2 rename not idempotent)"
printf '%s\n' "$blk" | grep -qF 'cp version.txt $(RULEDIR)/version.txt.gen' \
    || fail "cmd should read the SOURCE version.txt and write \$(RULEDIR)/version.txt.gen (self-copy bug)"

# 3b. the rule is NAMED after the renamed output (the bazel half below
# builds //:custom_command_version_txt_gen).
printf '%s\n' "$blk" | grep -qF 'name = "custom_command_version_txt_gen"' \
    || fail "rule should be named after the RENAMED output (custom_command_version_txt_gen)"

# 4. audit tag.
printf '%s\n' "$blk" | grep -q '"cmake-codegen-genrule-inplace-rewrite"' \
    || fail "genrule missing cmake-codegen-genrule-inplace-rewrite audit tag"

echo "ok  meta-cmake-genrule-inplace-rewrite: render OK — in-place output renamed off the source, cmd disambiguated"

# --- Bazel-build half: prove the rule that used to fail at load now builds ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-genrule-inplace-rewrite: bazel not on PATH, skipping build half"
    exit 0
fi
# Extract the first semver anywhere in the version output rather than the
# positional 2nd field: bazelisk's `--version` line isn't always
# `bazel <n>` (it can prefix its own wrapper banner), so awk '{print $2}'
# could read the wrong token and wrongly self-skip the build half. The
# semver grep is launcher-agnostic.
bazel_major=$("$BZL" --version 2>&1 | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1 | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "ok  meta-cmake-genrule-inplace-rewrite: bazel < 9, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$out_build" "$ws/BUILD.bazel"
cp "$fixture/foo.c" "$fixture/version.txt" "$ws/"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "inplace", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bzl_cache="$work_dir/.bazel"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bzl_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:custom_command_version_txt_gen) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: bazel build of the in-place-rewrite genrule failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if [ ! -f "$ws/bazel-bin/version.txt.gen" ]; then
    echo "FAIL: renamed output bazel-bin/version.txt.gen not produced"
    exit 1
fi

echo "ok  meta-cmake-genrule-inplace-rewrite: bazel build produces the renamed output (no input==output collision)"
