#!/bin/sh
# meta-cmake-out-of-tree-module-codegen.sh — render+build gate for best-effort
# recovery of codegen ISSUED from an out-of-tree cmake module.
#
# The fixture's project includes a cmake module (GenValue.cmake) that lives
# OUTSIDE the source root (a sibling dir on CMAKE_MODULE_PATH). That module runs
# an execute_process driving the project's OWN in-tree tool (tool.py) to write a
# source into the project's build dir. Because the call is ISSUED from out of the
# source tree, the in-source-tree lift filter skips it — so without the
# out-of-tree-module rescue the generated source has "no traced producer" and is
# BAKED from its on-disk bytes (and rejected under --bake-in=reject).
#
# Under --fidelity best-effort the call carries a project signal (it reads an
# in-tree source and writes a build-dir output), so it is EXTRACTED to a
# regenerating genrule instead. This gate asserts the genrule (not a bake) and
# builds AND runs //:app (exit 0 == gen_value() returns 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-out-of-tree-module-codegen"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture/proj" \
    --fidelity best-effort \
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

# The out-of-tree-module codegen is EXTRACTED to a regenerating genrule running
# the in-tree tool, NOT baked from the on-disk bytes.
grep -qE 'python3 \$\(location tool.py\)' "$build" \
    || fail "the out-of-tree-module codegen was not extracted to a tool genrule"
grep -qF 'outs = ["gen/value.c"]' "$build" \
    || fail "gen/value.c not declared by the extracted genrule"
grep -qF 'baked_gen_value_c' "$build" \
    && fail "gen/value.c was baked from on-disk bytes instead of extracted (the rescue didn't fire)"

echo "ok  meta-cmake-out-of-tree-module-codegen: out-of-tree-module codegen extracted to a regenerating genrule (not baked)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-out-of-tree-module-codegen: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-out-of-tree-module-codegen: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/proj/main.c "$fixture"/proj/tool.py "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "oot_module_codegen", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the extracted out-of-tree-module genrule didn't produce the output?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — extracted genrule produced the wrong value"
    exit 1
fi

echo "ok  meta-cmake-out-of-tree-module-codegen: //:app builds + runs from the extracted out-of-tree-module genrule"
