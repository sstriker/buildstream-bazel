#!/bin/sh
# meta-cmake-execute-process-unspecified-crossboundary.sh — render+build gate for
# the CROSS-BOUNDARY dir-operand lift: a nested satellite runs
# `tar -xf <archive> -C <dir>` where the extraction directory is in the OUTER
# build tree (OUTER_BUILD/ext), and the OUTER app consumes an extracted source.
#
# The dir-operand lift detects the argv directory operand and enumerates its
# on-disk files as the tool's outputs. Both the detection (argvDirOperands) and
# the enumeration (liftDirOperandOutputs) must consult the OUTER build dir
# (dirUnderBuildRoots) — checking the satellite's own build dir alone misses the
# cross-boundary dir → the lift declines and the element falls to not-lifted.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts the tool lifts
# to a `tar -xf … -C $(RULEDIR)/ext` genrule producing ext/gen.c (NOT declined),
# then builds AND runs //:app (exit 0 == gen_value() returns 7).
#
# Gating: skips cleanly when cmake, ninja, or tar isn't on PATH.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

for t in cmake ninja tar; do
    if ! command -v "$t" >/dev/null 2>&1; then
        echo "skip: $t not on PATH"
        exit 0
    fi
done

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/execute-process-unspecified-crossboundary"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --recognize-codegen \
    --cmake-script-trace \
    --out-build "$build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero (the cross-boundary dir-operand tool was not lifted → declined?)"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    exit 1
}

grep -qF '"ext/gen.c"' "$build" \
    || fail "ext/gen.c was not produced (the cross-boundary dir operand was not detected/enumerated across the outer build dir)"
grep -qE 'tar -xf .* -C \$\(RULEDIR\)/ext' "$build" \
    || fail "the dir-operand tool did not lift (expected a tar -xf … -C \$(RULEDIR)/ext genrule)"

echo "ok  meta-cmake-execute-process-unspecified-crossboundary: cross-boundary dir operand detected + enumerated across the outer build dir → lifted (not declined)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-execute-process-unspecified-crossboundary: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-execute-process-unspecified-crossboundary: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws/satellite"
cp "$fixture"/main.c "$ws/"
cp "$fixture"/satellite/payload.tar "$ws/satellite/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "unspecified_crossboundary", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the lifted cross-boundary dir-operand genrule didn't produce ext/gen.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
app_bin="$(find -L "$ws/bazel-bin" -name app -type f 2>/dev/null | head -1)"
if [ -z "$app_bin" ]; then
    app_bin="$ws/bazel-bin/app"
fi
if ! "$app_bin"; then
    echo "FAIL: app exited non-zero — cross-boundary extracted content wrong"
    exit 1
fi

echo "ok  meta-cmake-execute-process-unspecified-crossboundary: //:app builds + runs from the lifted cross-boundary dir-operand genrule"
