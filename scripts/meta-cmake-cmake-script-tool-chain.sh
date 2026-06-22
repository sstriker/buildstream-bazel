#!/bin/sh
# meta-cmake-cmake-script-tool-chain.sh — render+build gate for the multi-stage
# tool-chain recovery with a NON-anchorable (system-tempdir) intermediate.
#
# The fixture's custom command runs `cmake -P gen.cmake`, whose script runs two
# tool stages: stageA reads input.txt and writes <tmp>/int.tmp into a system
# tempdir (mktemp -d, outside the build dir); stageB reads <tmp>/int.tmp and
# writes the declared output. The per-step recovery can't anchor the /tmp
# intermediate — it would leak the absolute convert-time path into stageB's
# genrule and never produce it. The chain recovery folds BOTH stages into ONE
# genrule with int.tmp a CWD transient (the `mktemp -d` side-effect stage drops
# out), so the pipeline regenerates at Bazel build time with no leak.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts a single
# tool-chain genrule producing the declared output, the lifted cwd-relative
# int.tmp, no /tmp leak, then builds AND runs //:app (exit 0 == gen_value()
# returns 42 from the regenerated chain: input 21 -> *2 -> 42).
#
# Gating: skips cleanly when cmake / python3 aren't on PATH.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo "skip: python3 not on PATH"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-tool-chain"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
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

# The chain folds into ONE tool-chain genrule producing the declared output,
# with the intermediate a cwd-relative transient — no per-step int.tmp output,
# no absolute /tmp leak.
grep -qF 'cmake-codegen-tool-chain' "$build" \
    || fail "the tool chain was not folded (expected a cmake-codegen-tool-chain genrule)"
grep -qF 'outs = ["gen/value.c"]' "$build" \
    || fail "gen/value.c not declared by the folded chain genrule"
grep -qF 'int.tmp ' "$build" \
    || fail "the intermediate is not a cwd-relative transient (int.tmp) in the folded cmd"
grep -qF '/tmp/' "$build" \
    && fail "a system-tempdir path leaked into the BUILD file"
grep -qF 'mktemp' "$build" \
    && fail "the dead mktemp side-effect stage leaked into the folded cmd"

echo "ok  meta-cmake-cmake-script-tool-chain: 2-stage pipeline folded to one genrule (transient int.tmp, no /tmp leak)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-cmake-script-tool-chain: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-cmake-script-tool-chain: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/stageA.py "$fixture"/stageB.py "$fixture"/input.txt "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "toolchain", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the folded tool-chain genrule didn't produce the output?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — folded chain produced the wrong value"
    exit 1
fi

echo "ok  meta-cmake-cmake-script-tool-chain: //:app builds + runs from the folded tool-chain genrule"
