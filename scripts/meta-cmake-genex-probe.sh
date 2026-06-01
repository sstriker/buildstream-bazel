#!/bin/sh
# meta-cmake-genex-probe.sh — render gate for the Phase 3
# generator-parity probe-as-oracle path (ROADMAP.md "Phase 3 —
# genex-probe TOP_LEVEL_INCLUDES extension").
#
# Proves that a generator expression internal/genexeval's offline
# (a) Go-side evaluator REFUSES — here $<TARGET_OBJECTS:obj> in a
# file(GENERATE) CONTENT body — is now RESOLVED end-to-end via the
# cmake probe hook (--probe-genex). cmake's own generator-phase
# evaluator answers $<TARGET_OBJECTS:obj> at generation time; the
# probe captures the resolved object list; ReadGenexProbe feeds it
# into the lift Context; the lifter resolves the genex instead of
# baking cmake's rendered bytes.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/genex-probe (an OBJECT_LIBRARY
# `obj`, a consumer STATIC_LIBRARY, and a file(GENERATE) whose
# CONTENT embeds $<TARGET_OBJECTS:obj>).
#
# Asserts on the emitted cmake_configure_file rule for the
# file(GENERATE) output:
#   1. It carries cmake-codegen-genex-resolved (Phase 3 collapsed
#      tag) — the genex was resolved, not baked.
#   2. It does NOT carry cmake-codegen-genex-unresolved — no
#      Tier-1 refusal / legacy bytes-embedded fallback.
#   3. It carries genex_context = (the (a) evaluator path the probe
#      feeds) and target_objects = {":obj": "obj"} (the Bazel-time
#      object-list wire), and the template body —
#      $<TARGET_OBJECTS:obj> — rides as the readable inline content
#      attribute, NOT the rendered .o path list.
#
# This is the live counterpart of the Go-side round-trip pinned by
# TestReadGenexProbe_EmptyConfig (reader) and
# TestProbeGenex_ObjectLibrary_LiveCMake (hook emission). cmake
# 3.24+ is required for the CMAKE_PROJECT_TOP_LEVEL_INCLUDES hook;
# the gate skips cleanly when cmake isn't on PATH.

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

fixture="$repo_root/converter/testdata/sample-projects/genex-probe"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    --probe-genex \
    --lift-configure-file=true \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero with --probe-genex"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

# Slice the gen_obj_manifest_txt genrule block so the assertions
# are scoped to that rule.
blk="$(awk '
    /name = "gen_obj_manifest_txt"/ { in_blk = 1 }
    in_blk { print }
    in_blk && /# keep/ { exit }
' "$out_build")"

if [ -z "$blk" ]; then
    echo "FAIL: gen_obj_manifest_txt genrule missing from BUILD.bazel"
    sed 's/^/   /' "$out_build"
    exit 1
fi

fail() {
    echo "FAIL: $1"
    echo "--- gen_obj_manifest_txt block ---"
    printf '%s\n' "$blk" | sed 's/^/   /'
    exit 1
}

# 1. Resolved tag present.
printf '%s\n' "$blk" | grep -q '"cmake-codegen-genex-resolved"' \
    || fail "gen_obj_manifest_txt missing cmake-codegen-genex-resolved — the probe didn't resolve \$<TARGET_OBJECTS:obj>"

# 2. Unresolved-fallback tag absent.
printf '%s\n' "$blk" | grep -q '"cmake-codegen-genex-unresolved"' \
    && fail "gen_obj_manifest_txt carries cmake-codegen-genex-unresolved — genex fell back to legacy bytes instead of resolving via the probe"

# 3. (a) evaluator + TARGET_OBJECTS Bazel-time wire present, as readable
# rule attributes (the cmake_configure_file lift carries genex_context as
# a JSON string and target_objects as a label-keyed dict — no base64,
# no $(locations) shell wire).
printf '%s\n' "$blk" | grep -q -- 'genex_context =' \
    || fail "gen_obj_manifest_txt missing genex_context = ((a) evaluator wire)"
printf '%s\n' "$blk" | grep -q -- '":obj": "obj"' \
    || fail "gen_obj_manifest_txt missing target_objects {\":obj\": \"obj\"} (Bazel-time object-list wire)"

# 3b. The template body (the literal genex) rides as the readable inline
# `content` attribute, NOT cmake's rendered .o path. The body is emitted
# verbatim (Go %q does not escape $ < >), so assert it still contains the
# literal $<TARGET_OBJECTS:obj>.
printf '%s\n' "$blk" | grep -q -- 'content =' \
    || fail "gen_obj_manifest_txt missing inline content ="
printf '%s\n' "$blk" | grep -qF -- '$<TARGET_OBJECTS:obj>' \
    || fail "content should carry the literal \$<TARGET_OBJECTS:obj> template, not the rendered object path (which would mean a bytes-baked fallback)"

echo "ok  meta-cmake-genex-probe: \$<TARGET_OBJECTS:obj> in file(GENERATE) resolved via --probe-genex (cmake-codegen-genex-resolved, no Tier-1 refusal)"
