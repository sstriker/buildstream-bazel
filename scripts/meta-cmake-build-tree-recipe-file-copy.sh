#!/bin/sh
# meta-cmake-build-tree-recipe-file-copy.sh — render+build gate for finding G5
# [13]: a file(COPY) ISSUED from a generated+include()d recipe `.cmake` in the
# BUILD tree is recovered as a regenerating cp-genrule, not dropped to a frozen
# on-disk byte-bake.
#
# The fixture writes a recipe `.cmake` into the build dir at configure time and
# include()s it; the recipe's file(COPY) copies a committed source into the
# build dir, where the executable consumes it. The file(COPY) event is recorded
# as issued from the build-tree recipe, so the old inSourceTree gate on
# ExtractFileWriterCalls dropped it — the byte-bake froze the copied bytes. The
# fix gates on inProjectScope (build-tree-aware, mirroring the sibling
# configure_file / file(GENERATE) extractors), so the copy upgrades to a TRUE cp
# lift that re-runs at Bazel build time with the committed source declared.
#
# Asserts the regenerating cp-genrule (the file-writer-copy facet, the committed
# payload.c as its declared src) renders, asserts the copied output is NOT a
# frozen build-dir bake, then bazel-builds and RUNS //:app (exit 0 == the copied
# payload() returns 7).
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

fixture="$repo_root/converter/testdata/sample-projects/cmake-build-tree-recipe-file-copy"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

build="$work_dir/BUILD.bazel"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$build" \
    --conversion-todos-report "$work_dir/todos.json" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- BUILD.bazel ---"
    sed 's/^/   /' "$build" 2>/dev/null || true
    echo "   --- convert.stderr ---"
    sed 's/^/   /' "$work_dir/convert.stderr" 2>/dev/null || true
    exit 1
}

# The build-tree-recipe-issued file(COPY) is recovered as a regenerating cp
# genrule from the committed source, NOT a frozen on-disk bake.
grep -qF 'out = "copied/payload.c"' "$build" \
    && fail "the recovered output should be declared via the genrule outs list, not an inline out= bake"
grep -qF 'outs = ["copied/payload.c"]' "$build" \
    || fail "copied/payload.c not declared by a regenerating genrule (build-tree recipe copy dropped?)"
grep -qF 'cp \"$(location payload.c)\"' "$build" \
    || fail "the file(COPY) was not lifted to a cp genrule from the committed source"
grep -qF '"cmake-codegen-file-writer-copy"' "$build" \
    || fail "the copy-lift facet is missing (the copy didn't upgrade to a true lift)"

# The copied output must NOT be a frozen build-dir / file-writer bake. Scope the
# negative grep to the genrule's cmd/outs/tags lines so a carried CMakeLists
# comment mentioning "bake" can't trip it.
if grep -E '^\s*(cmd|outs|tags|out) *=' "$build" \
        | grep -qE 'cmake-codegen-build-dir-bake|cmake-codegen-file-writer-bake'; then
    fail "copied/payload.c was baked (cmake-codegen-*-bake on a producer line) instead of cp-lifted"
fi
# The consumer attaches the recovered build-dir source.
grep -qF '"copied/payload.c",' "$build" \
    || fail "the copied build-dir source is not attached to the consumer"

echo "ok  meta-cmake-build-tree-recipe-file-copy: build-tree-recipe file(COPY) recovered as a regenerating cp-genrule (not a frozen bake)"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-build-tree-recipe-file-copy: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-build-tree-recipe-file-copy: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
# payload.c is now a DECLARED INPUT of the cp lift (the upgrade under test), so
# the workspace must carry it — the frozen bake never needed it.
cp "$fixture"/main.c "$fixture"/payload.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "build_tree_recipe_file_copy", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:app failed (the recovered cp-genrule didn't produce copied/payload.c?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — the cp-lifted source compiled/linked to the wrong value"
    exit 1
fi

echo "ok  meta-cmake-build-tree-recipe-file-copy: //:app builds + runs from the recovered cp-genrule"
