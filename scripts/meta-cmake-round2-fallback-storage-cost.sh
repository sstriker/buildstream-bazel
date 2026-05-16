#!/bin/sh
# meta-cmake-round2-fallback-storage-cost.sh — fixture-driven
# signal for the **storage-duplication cost** the ROADMAP's
# "Repo-rule install for kind:cmake round-2 fallback" entry calls
# out: the round-2 fallback shape ships install_tree.tar between
# project B and project A's BUILD.bazel.out AND extracts a
# subset of its contents via a per-element _install_tree_extract
# genrule, costing CAS roughly tar_bytes + Σ(per-target artifact
# bytes from each cc_import / sh_binary's transitive paths).
#
# This gate doesn't fix anything — it makes the cost
# *measurable* at render time so the maintainer can re-evaluate
# the repo-rule alternative against real numbers, not hand-wavy
# "roughly 2×" estimates.
#
# What it measures (render-time, no bazel required):
#
#   - The number of paths the extract genrule duplicates out of
#     install_tree.tar (counted as the `_install_tree_extract`
#     genrule's outs lines minus the per-genrule overhead).
#   - The per-target stub count (cc_import + sh_binary + cc_library
#     stubs with the cmake-codegen-execute-process-fallback tag).
#
# What it doesn't measure (would need a real bazel build):
#
#   - Absolute install_tree.tar byte size — depends on the actual
#     binaries the build produces (compiler / linker variants).
#   - The full CAS-stored bytes (tar + Directory entries the
#     extract genrule materialises).
#
# The fixture is a small cmake project with an unliftable
# execute_process (echo-stamp, classifier-equivalent to git
# rev-parse + OUTPUT_VARIABLE → BucketStamp) and a mix of
# FILE_SET HEADERS (modern cmake; duplicated by the extract
# genrule) and install(FILES ...) (legacy; stays in
# install_tree.tar only). The mix matters: it shows that the
# duplication factor isn't a flat 2× on the whole install tree —
# only the entries the per-target stubs reference get duplicated.
# That's a useful corrective to the ROADMAP's "roughly 2×"
# wording: the real cost depends on the install-tree shape's mix
# of artifact-bearing vs documentation-only entries.
#
# To scale this signal up for a real evaluation, point the gate
# at a representative FDSDK-scale project (LLVM, mesa, etc.) and
# read off the extract genrule's outs count; multiply by the
# per-element count to size the total CAS duplication for a
# fleet.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

fixture_src="$repo_root/converter/testdata/sample-projects/execute-process-unliftable-fallback"
fixture_reply="$repo_root/converter/testdata/fileapi/execute-process-unliftable-fallback"

# === Step 1: strict mode refuses with the typed Tier-1 code. ===
# Without --unsupported-execute-process-fallback, the converter
# exits Tier-1 on the unliftable execute_process call. This pins
# the contract the fallback flag opts out of — operators who
# don't want the placeholder shape get a clean refusal.
strict_log="$work_dir/strict.log"
if "$bin_dir/convert-element-cmake" \
    --source-root="$fixture_src" \
    --reply-dir="$fixture_reply" \
    --out-build="$work_dir/strict-BUILD.out" >"$strict_log" 2>&1; then
    echo "storage-cost: strict-mode convert-element-cmake unexpectedly SUCCEEDED" >&2
    cat "$strict_log" >&2
    exit 1
fi
if ! grep -qF 'unsupported-execute-process' "$strict_log"; then
    echo "storage-cost: strict-mode refusal missing typed code 'unsupported-execute-process':" >&2
    cat "$strict_log" >&2
    exit 1
fi
echo "storage-cost: strict mode refuses with unsupported-execute-process (contract pinned)"

# === Step 2: fallback mode emits the placeholder shape. ===
fallback_build="$work_dir/fallback-BUILD.out"
"$bin_dir/convert-element-cmake" \
    --source-root="$fixture_src" \
    --reply-dir="$fixture_reply" \
    --out-build="$fallback_build" \
    --unsupported-execute-process-fallback=true >"$work_dir/fallback.log" 2>&1

# Sanity-check markers — the placeholder shape's identifying
# rules + tag.
for marker in \
    'name = "_install_tree_extract"' \
    '"install_tree.tar"' \
    'cmake-codegen-execute-process-fallback' \
    'cc_import(' \
    'sh_binary('; do
    if ! grep -qF -- "$marker" "$fallback_build"; then
        echo "storage-cost: fallback BUILD missing marker: $marker" >&2
        cat "$fallback_build" >&2
        exit 1
    fi
done

# === Step 3: count the storage signal. ===
# Lines in the _install_tree_extract genrule's outs = files the
# extract pulls out of install_tree.tar (and which therefore
# land in CAS twice — once inside the tar blob, once as
# separate Directory entries the per-target stubs reference).
# We extract the outs block via awk's range; matches what
# buildifier renders (one path per line, indented).
extract_outs=$(awk '
    /name = "_install_tree_extract"/ { in_rule = 1 }
    in_rule && /outs = \[/ { in_outs = 1; next }
    in_rule && in_outs && /^[[:space:]]*\],/ { in_outs = 0; in_rule = 0 }
    in_rule && in_outs && /"install_tree\// { print }
' "$fallback_build" | wc -l | tr -d ' ')

# Count per-target stubs (cc_import + sh_binary instances). The
# stubs are what reference the extracted files; one stub per
# Target.Type with an install destination.
stub_count=$(grep -cE '^(cc_import|sh_binary|cc_library)\(' "$fallback_build" || true)

echo "storage-cost: fixture render emits $extract_outs extract-genrule outs across $stub_count per-target stubs"

# Spot-check: extract_outs should be > 0 (this fixture has 3
# artifact-bearing targets with FILE_SET HEADERS contributing
# extra entries). Exact count drift between cmake versions is
# fine, so we don't pin it; the >0 check catches a regression
# that would silently make the cost signal vanish.
if [ "$extract_outs" -lt 3 ]; then
    echo "storage-cost: extract genrule outs count ($extract_outs) below expected floor (3 artifacts: lib + lib + bin)" >&2
    cat "$fallback_build" >&2
    exit 1
fi

# === Step 4: surface the duplication math. ===
# install_tree.tar is staged once per element; each extract
# genrule output is a CAS Directory entry duplicating its tar
# content. The duplication factor is:
#
#   total_CAS_bytes  ≈  tar_bytes + Σ(extract_outs file bytes)
#
# For this fixture (artifact mix: 2 libs + 1 binary + 2 headers
# via FILE_SET, 1 header via install(FILES) stays in tar only):
#
#   extract_outs = N=5 (3 artifacts + 2 FILE_SET headers)
#   in-tar-only  = 1 (baz.h via legacy install(FILES))
#   ratio       = 5 / (5 + 1) ≈ 83% of tar bytes are duplicated
#
# At FDSDK scale (hundreds of installed files per element, many
# elements), the absolute CAS duplication adds up:
# tar_bytes × N_elements + extract_bytes × N_consumers. The
# repo-rule alternative the ROADMAP describes would eliminate
# the extract leg, leaving only the tar (or, in the
# loading-time-cmake variant, neither). Whether that's worth
# the trade-offs (Bazel-startup blocking, RBE-incompatibility,
# host-toolchain hermeticity erosion) depends on per-fleet
# numbers an operator can derive by running this gate against
# their own meta-project.
echo "storage-cost: signal summary —"
echo "  extract-genrule outs duplicated in CAS: $extract_outs"
echo "  per-target stubs referencing extracts:  $stub_count"
echo "  (run against a larger fixture to size the fleet cost; see header comment)"

echo "storage-cost: ok"
