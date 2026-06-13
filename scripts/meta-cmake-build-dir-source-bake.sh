#!/bin/sh
# meta-cmake-build-dir-source-bake.sh — render+build gate for the
# no-silent-drops contract on configure-written build-dir files.
#
# The fixture writes build-dir SOURCES and a HEADER through untraced
# writers (`file(WRITE)`, `file(COPY)`, `file(TOUCH)`) and feeds them to
# an executable — historically every one of these was silently elided
# (tag-only accounting), producing a BUILD whose binary compiled from
# main.c alone, linked by luck or failed downstream with no converter
# signal.
#
# The contract under test: anything the codemodel references is either
# RECOVERED or accounted LOUDLY. Here the configure already ran, so the
# bytes exist in the live build dir and every file bakes via the on-disk
# bake (cmake-codegen-build-dir-bake facet, convert-time-bake warning +
# `bake` todos); the header reaches the consumer through the
# demand-driven build-dir include walk. Nothing is dropped, so the
# `source-elided` channel must stay SILENT.
#
# Asserts the baked shapes render, then bazel-builds and RUNS the
# binary — exit 0 proves all three baked sources compiled+linked and the
# baked header carries the right value (a()+b()+D_VALUE == 7).

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

fixture="$repo_root/converter/testdata/sample-projects/build-dir-source-bake"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$work_dir/BUILD.bazel" \
    --conversion-todos-report "$work_dir/todos.json" \
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
    echo "   --- convert.stderr ---"
    sed 's/^/   /' "$work_dir/convert.stderr" 2>/dev/null || true
    exit 1
}

# The writer index ties each file to its traced producer:
# WRITE/TOUCH content materializes from the TRACE (writer facet,
# provenance-cited), and file(COPY) upgrades to a TRUE cp lift that
# re-runs at Bazel build time with the committed source declared.
grep -qF 'out = "gen_a.c"' "$build" || fail "file(WRITE) build-dir source not recovered"
grep -qF 'cp \"$(location b.c)\"' "$build" || fail "file(COPY) not lifted to a cp genrule from the committed source"
grep -qF '"cmake-codegen-file-writer-copy"' "$build" || fail "copy lift facet missing"
grep -qF 'out = "gen_c.c"' "$build" || fail "file(TOUCH) empty build-dir source not recovered"
grep -qF 'out = "gen_d.h"' "$build" || fail "file(WRITE) build-dir header not recovered (include-walk recovery)"
grep -qF '"cmake-codegen-file-writer-bake"' "$build" || fail "file-writer bake facet missing"
grep -q 'copy_copied_b_c' "$work_dir/convert.stderr" \
    && fail "the cp lift must NOT ride the convert-time-bake inventory (it re-runs at build time)"

# The consumer's srcs carry every baked file (sources + walked header).
for s in '"gen_a.c"' '"copied/b.c"' '"gen_c.c"' '"gen_d.h"'; do
    grep -qF "$s," "$build" || fail "baked file $s not attached to the consumer"
done

# Recovered means recovered: the loud-drop channel stays silent.
grep -q 'DROPPED without recovery' "$work_dir/convert.stderr" \
    && fail "source-elided warning fired although every file baked"
grep -q '"kind": "source-elided"' "$work_dir/todos.json" \
    && fail "source-elided todo emitted although every file baked"

# The bake trade is accounted: convert-time-bake warning + bake todos.
grep -q 'convert-time-baked output' "$work_dir/convert.stderr" \
    || fail "convert-time-bake warning missing for the build-dir bakes"
[ "$(grep -c '"kind": "bake"' "$work_dir/todos.json")" -ge 3 ] \
    || fail "expected >=3 bake todos (the COPY upgraded to a true lift)"

echo "ok  meta-cmake-build-dir-source-bake: configure-written build-dir files baked — nothing silently dropped"

# --- Bazel-build half ---
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "ok  meta-cmake-build-dir-source-bake: bazel not on PATH, skipping build half"
    exit 0
fi
if ! bazel_version_out=$("$BZL" version 2>&1); then
    echo "FAIL: '$BZL version' failed:"; printf '%s\n' "$bazel_version_out" | sed 's/^/   /'; exit 1
fi
bazel_major=$(printf '%s\n' "$bazel_version_out" | awk -F': ' '/^Build label:/{print $2; exit}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 7 ]; then
    echo "ok  meta-cmake-build-dir-source-bake: bazel < 7, skipping build half"
    exit 0
fi

ws="$work_dir/ws"
mkdir -p "$ws"
# b.c is now a DECLARED INPUT of the cp lift (the upgrade under test),
# so the workspace must carry it — the bake never needed it.
cp "$fixture"/main.c "$fixture"/b.c "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "builddirsourcebake", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

bz_cache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bz_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:app) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the converted build-dir-source-bake project failed"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi
# The binary's exit code IS the content check: 0 only when all three
# baked sources linked AND the baked header carries D_VALUE=4.
if ! "$ws/bazel-bin/app"; then
    echo "FAIL: app exited non-zero — a baked source or the baked header content is wrong"
    exit 1
fi

echo "ok  meta-cmake-build-dir-source-bake: baked build-dir files compile, link, and run clean (no cmake at build time)"
