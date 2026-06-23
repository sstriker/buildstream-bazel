#!/bin/sh
# meta-cmake-cmake-script-tempdir-relocate-copydir.sh — render+build gate for
# the temp-dir-then-copy codegen recovery with the `cmake -E copy_directory`
# relocation form (a recursive tree copy, no per-file operand).
#
# The fixture's custom command runs `cmake -P gen.cmake`, whose script runs a
# tool with WORKING_DIRECTORY=<tmp> that writes BOTH a.c and b.c there and then
# relocates the whole tree into the declared output dir with `cmake -E
# copy_directory <tmp> <OUTDIR>`. copy_directory enumerates no per-file operand,
# so the recovery expands it against the edge's DECLARED outputs (each declared
# output under destdir maps to srcdir/<its rel path>) and recovers the
# regenerating TOOL — a genrule that runs `python3 tool.py` and relocates each
# cwd output to $(RULEDIR) — instead of FREEZING each destination's bytes.
#
# Converted with --recognize-codegen --cmake-script-trace. Asserts a TOOL
# genrule (driver=python3) producing BOTH declared outputs with a lifted copy
# each, NOT a frozen copy bake, then builds AND runs //:app (exit 0 ==
# gen_a()+gen_b() == 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-script-tempdir-relocate-copydir"
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

# The TOOL is recovered to ONE regenerating genrule producing BOTH declared
# outputs (the copy_directory tree expanded per-file), NOT frozen-baked.
grep -qF 'cmake-codegen-driver=python3' "$build" \
    || fail "the codegen tool was not recovered (expected a python3-driver genrule)"
grep -qF 'cp a.c $(RULEDIR)/gen/a.c' "$build" \
    || fail "the lifted copy for a.c (tempdir tree -> \$(RULEDIR)/declared) is missing"
grep -qF 'cp b.c $(RULEDIR)/gen/b.c' "$build" \
    || fail "the lifted copy for b.c (tempdir tree -> \$(RULEDIR)/declared) is missing"
grep -q 'cmake-codegen-execute-process-op=copy' "$build" \
    && fail "the copy was frozen-baked (op=copy write_file) instead of recovering the tool"

echo "ok  meta-cmake-cmake-script-tempdir-relocate-copydir: copy_directory expanded per-declared-output; tool recovered to one regenerating genrule + per-file lifted copies"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-cmake-script-tempdir-relocate-copydir: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-cmake-script-tempdir-relocate-copydir: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/main.c "$fixture"/tool.py "$fixture"/gen.cmake "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "tdrelocatecd", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered temp-dir tool genrule didn't produce both outputs?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — recovered tool output content wrong"
    exit 1
fi

echo "ok  meta-cmake-cmake-script-tempdir-relocate-copydir: //:app builds + runs from the recovered temp-dir tool genrule"
