#!/bin/sh
# meta-cmake-cmake-script-tempdir-relocate-rename.sh — render+build gate for the
# temp-dir-then-copy codegen recovery .
#
# The fixture's custom command runs `cmake -P gen.cmake`, whose script runs a
# tool with WORKING_DIRECTORY=<tmp> (the tool writes value.c there, naming no
# final path in argv) and then `cmake -E copy <tmp>/value.c <declared OUT>`.
# Without the temp-dir-relocate recovery the only lift FREEZES the copy's
# destination bytes (a write_file snapshot via bakeBuildDirCopyOutput). The
# recovery instead recovers the regenerating TOOL — for this unrecognized tool,
# a genrule that runs `python3 tool.py` and relocates its cwd output to
# $(RULEDIR) — so the output re-derives at Bazel build time.
#
# Converted with --recognize-codegen --cmake-script-trace (the script re-trace
# opt-ins). Asserts a TOOL genrule (driver=python3) producing the declared
# output, NOT a frozen copy bake (op=copy write_file), then builds AND runs
# //:app (exit 0 == gen_value() returns 7).
#
# Gating: skips cleanly when cmake / python3 aren't on PATH (the re-trace runs
# cmake -P at convert time; the tool is python3).

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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-tempdir-relocate-rename"
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

# The TOOL is recovered to a regenerating genrule, NOT frozen-baked as the copy.
grep -qF 'cmake-codegen-driver=python3' "$build" \
    || fail "the codegen tool was not recovered (expected a python3-driver genrule)"
grep -qF 'outs = ["gen/value.c"]' "$build" \
    || fail "gen/value.c not declared by the recovered tool genrule"
grep -qF 'cp value.c $(RULEDIR)/gen/value.c' "$build" \
    || fail "the lifted copy (tempdir output -> \$(RULEDIR)/declared) is missing"
grep -q 'cmake-codegen-execute-process-op=copy' "$build" \
    && fail "the copy was frozen-baked (op=copy write_file) instead of recovering the tool"

echo "ok  meta-cmake-cmake-script-tempdir-relocate-rename: temp-dir tool recovered to a regenerating genrule + lifted copy (no frozen copy-bake)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-cmake-script-tempdir-relocate-rename: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-cmake-script-tempdir-relocate-rename: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/tool.py "$fixture"/gen.cmake "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "tdrelocatern", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered temp-dir tool genrule didn't produce the output?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered tool output content wrong"
    exit 1
fi

echo "ok  meta-cmake-cmake-script-tempdir-relocate-rename: //:app builds + runs from the recovered temp-dir tool genrule"
