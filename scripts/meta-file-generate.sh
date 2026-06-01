#!/bin/sh
# meta-file-generate.sh — render gate for the file(GENERATE)
# lifter (Commit 3/4/5 of the lift-GENERATED PR).
#
# Runs convert-element-cmake (live, --source-root) against the
# file-generate sample project and asserts the rendered BUILD.bazel
# contains the expected cmake_configure_file rule shapes for all three
# lift modes:
#
#   - INPUT form, genex-free → lifted with a `template` label +
#     cmake-configure-file `tool`, cmake-codegen-lifted tag.
#   - CONTENT form, genex-free → lifted with inline `content`,
#     cmake-codegen-lifted tag.
#   - CONTENT form with `$<CONFIG>` (a configure-time-resolvable
#     genex whose static surround uniquely anchors the resolved
#     value in cmake's rendered output) → lifted via the (a)
#     evaluator or (b) structured-replay path: the rule carries a
#     readable `genex_context` JSON (a) or `genex_values` dict (b)
#     mapping each top-level `$<...>` literal to the bytes cmake
#     emitted at generate-time. Both cmake-codegen-lifted and
#     cmake-codegen-genex-resolved (Phase 3 tag collapse) ride on
#     the rule's tags. Templates whose genex value can't be
#     anchored (extractor failure modes — see
#     converter/internal/lower/genex_extract.go) still fall
#     back to the legacy bytes-embedded shape, audit-tagged
#     `cmake-codegen-genex-unresolved`; the unit tests cover that
#     branch.
#
# The Go test (TestEmit_FileGenerate_Golden) covers the same
# round-trip with full byte-stability; this gate exercises the
# binary's CLI surface so a regression in convert-element-cmake's
# flag plumbing or output writer also catches.
#
# Why a separate gate: parallel to meta-element-fold.sh and the
# autotools / make round-2 gates, each focused on one lift
# axis. CI runs gates individually; a single failing gate's
# diagnostic surface stays narrow.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# Live --source-root mode runs cmake (which selects the Ninja generator);
# skip cleanly when either tool is absent.
if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi
if ! command -v ninja >/dev/null 2>&1; then
    echo "skip: ninja not on PATH (--source-root mode uses the Ninja generator)"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Live --source-root mode (runs cmake against the in-repo sample project).
# --source-root is mutually exclusive with --reply-dir, and only --source-root
# sets HostSourceRoot — the root the INPUT-form file(GENERATE) template
# (src/version.h.in) is read from. The captured-reply fixture can't be used
# here: its codemodel records an ABSOLUTE recording-machine source path, so an
# offline --reply-dir run would read templates from that path and silently
# fall back to the legacy bake on any checkout root that doesn't match it
# (e.g. CI's /home/runner/...). Running live keeps the gate portable.
src="$repo_root/converter/testdata/sample-projects/file-generate"
out_build="$work_dir/BUILD.bazel"

"$bin_dir/convert-element-cmake" \
    --source-root "$src" \
    --lift-configure-file=true \
    --out-build "$out_build"

if [ ! -f "$out_build" ]; then
    echo "convert-element-cmake did not produce $out_build" >&2
    exit 1
fi

# Required content per lift mode.
#
# gen_config_tag_h is the genex-bearing call. Both the (a) Go-
# side evaluator and the (b) structured-base64 capture lifts
# have shipped (ROADMAP "Generator-expression evaluation in
# lifted genrules"). With the fixture's cmake-to-bazel.vars.dump
# now carrying CMAKE_BUILD_TYPE=Release, the (a) evaluator
# fires first: the captured Context (config + compiler_id +
# platform_id) rides as a readable `genex_context` attribute and
# cmake-configure-file re-evaluates $<CONFIG> at Bazel time.
# The rule carries BOTH cmake-codegen-lifted AND
# cmake-codegen-genex-resolved (Phase 3 tag collapse: the (a)
# evaluator and (b) capture share one -resolved tag; the
# (a)-vs-(b) split now lives only in the attribute set — genex_context
# for (a), genex_values for (b)). Templates whose ops (a)
# can't resolve fall through to (b); templates whose static
# surround can't anchor (b) extraction fall back to the legacy
# cmake-codegen-genex-unresolved shape — covered by the unit
# tests in converter/internal/lower/file_generate_test.go.
required=$(cat <<'EOF'
name = "gen_version_h"
"src/version.h.in"
"//tools:cmake-configure-file"
"cmake-codegen-lifted"
name = "gen_banner_h"
content =
name = "gen_config_tag_h"
"cmake-codegen-genex-resolved"
genex_context =
EOF
)

while IFS= read -r line; do
    if [ -z "$line" ]; then continue; fi
    if ! grep -qF -- "$line" "$out_build"; then
        echo "BUILD.bazel missing required substring: $line" >&2
        echo "--- BUILD.bazel ---" >&2
        cat "$out_build" >&2
        exit 1
    fi
done <<EOF
$required
EOF

# Negative: the (b)-lifted genex genrule must NOT carry the
# legacy-fallback `cmake-codegen-genex-unresolved` tag —
# that one means "extraction failed, rendered bytes still in
# srckey." A successful (b) lift gets the `-lifted` variant
# only, so an audit query targeting unresolved genex spend
# (the legacy tag) finds only the real fallbacks.
if awk '
    /name = "gen_config_tag_h"/ { in_blk = 1 }
    in_blk { print }
    in_blk && /^\)/ { exit }
' "$out_build" | grep -qE '"cmake-codegen-genex-unresolved"'; then
    echo "gen_config_tag_h must NOT carry the legacy-fallback cmake-codegen-genex-unresolved tag — the (a)/(b) lift should have resolved this fixture's genex" >&2
    exit 1
fi

# Determinism: re-run, byte-diff outputs.
out_build_2="$work_dir/BUILD.bazel.2"
"$bin_dir/convert-element-cmake" \
    --source-root "$src" \
    --lift-configure-file=true \
    --out-build "$out_build_2"

if ! diff -q "$out_build" "$out_build_2" >/dev/null; then
    echo "convert-element-cmake output not deterministic across runs" >&2
    diff -u "$out_build" "$out_build_2" >&2
    exit 1
fi

echo "meta-file-generate: ok"
