#!/bin/sh
# meta-cmake-execute-process-unspecified-outs.sh — render+build gate for
# the unspecified-output execute_process lift.
#
# `tool <input…>` whose outputs never appear in the argv. The recovery is
# DECLARATIVE — no convert-time re-execution, no per-tool tables: the File
# API's consumed build-dir sources are the demand side, ninja's output set
# is the exclusion, and the trace argv provides the linkage. Two classes:
#
#   - dir-operand containment (`tar -xf payload.tar -C <build>/ext`): the
#     on-disk files under the argv directory operand are the outs; the
#     re-run genrule rewrites the operand to $(RULEDIR)/ext, so the tool
#     is told where to write at Bazel time.
#   - derived-name correlation (`tar -xf data.tar` extracting data_gen.h
#     into the build-root cwd): the consumed orphan correlates to the
#     input by stem; the configure-written bytes bake (write_file).
#
# Asserts both shapes render, then bazel-builds the consumer binary and
# RUNS it — exit 0 proves both generated files carry the right content.

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

fixture="$repo_root/converter/testdata/sample-projects/execute-process-unspecified-outs"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$work_dir/BUILD.bazel" \
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

grep -qF 'outs = ["ext/gen.c"]' "$build" || fail "dir-operand output not lifted"
grep -qF -- '-C $(RULEDIR)/ext' "$build" || fail "dir operand not rewritten to \$(RULEDIR)/ext"
grep -qF 'mkdir -p \"$(RULEDIR)/ext\"' "$build" || fail "out dir not pre-created"
grep -qF 'cmake-codegen-execute-process-dir-outs' "$build" || fail "dir-outs audit facet missing"
grep -qF 'out = "data_gen.h"' "$build" || fail "derived-name orphan not baked"
grep -qF 'cmake-codegen-execute-process-derived-bake' "$build" || fail "derived-bake audit facet missing"
grep -qF '"ext/gen.c",' "$build" || fail "consumer does not reference the dir-class out"
grep -q "unsupported-" "$work_dir/convert.stderr" \
    && fail "converter emitted a typed refusal (both calls should lift)"

echo "ok  meta-cmake-execute-process-unspecified-outs: dir-operand + derived-name outputs recovered (no argv-declared outs)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-execute-process-unspecified-outs: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-execute-process-unspecified-outs: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/payload.tar "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "unspecouts", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //...) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the unspecified-outs targets failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# The binary's exit code IS the content check: gen_value()==42 comes from
# the genrule's tar re-run, DATA_GEN==7 from the baked header.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — generated content wrong"
    exit 1
fi

echo "ok  meta-cmake-execute-process-unspecified-outs: re-run genrule + baked header build and the consumer runs clean"
