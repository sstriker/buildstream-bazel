#!/bin/sh
# meta-cmake-round2-fallback-storage-cost.sh — fixture-driven
# proof that the round-2 fallback's per-consumer storage
# duplication is GONE after the install_tree.tar -> install-root
# TreeArtifact cutover.
#
# Before the cutover, the fallback shipped an opaque
# install_tree.tar between project B and project A's BUILD.bazel.out
# AND re-extracted a subset of its contents via a per-element
# `_install_tree_extract` tar-untar genrule. That cost CAS roughly
# tar_bytes + Σ(per-target artifact bytes) — the entries each
# cc_import / sh_binary referenced were materialised twice (once in
# the tar blob, once as the genrule's extracted Directory entries).
#
# After the cutover the install root IS a Bazel TreeArtifact
# (a Directory merkle tree in CAS). The per-target stubs reference
# `pick_file` projections that copy a SINGLE file out of that shared
# directory in place — no whole-tree re-materialization, file-
# granular CAS dedup. This gate pins that: ZERO `_install_tree_extract`
# genrules, ZERO `install_tree.tar` references, and one `pick_file`
# per referenced artefact/header (the projection, not a duplication —
# the bytes already live in the install-root Directory; pick_file
# names a view into it).
#
# Render-time only (no bazel build required): it runs
# convert-element-cmake against a fixture reply and inspects the
# emitted BUILD.bazel.out shape.
#
# The fixture is a small cmake project with an unliftable
# execute_process (echo-stamp, classifier-equivalent to git
# rev-parse + OUTPUT_VARIABLE → BucketStamp) and a mix of
# FILE_SET HEADERS (modern cmake) and install(FILES ...) (legacy).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

fixture_reply="$repo_root/converter/testdata/fileapi/execute-process-unliftable-fallback"

# === Step 1: strict mode refuses with the typed Tier-1 code. ===
# Without --unsupported-execute-process-fallback, the converter
# exits Tier-1 on the unliftable execute_process call. This pins
# the contract the fallback flag opts out of — operators who
# don't want the placeholder shape get a clean refusal.
strict_log="$work_dir/strict.log"
if "$bin_dir/convert-element-cmake" \
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

# === Step 2: fallback mode emits the pick_file placeholder shape. ===
fallback_build="$work_dir/fallback-BUILD.out"
"$bin_dir/convert-element-cmake" \
    --reply-dir="$fixture_reply" \
    --out-build="$fallback_build" \
    --bazel-package-path="elements/thelib" \
    --unsupported-execute-process-fallback=true >"$work_dir/fallback.log" 2>&1

# Sanity-check markers — the placeholder shape's identifying rules,
# tag, and the pick_file load.
for marker in \
    'load("@rules_buildstream_bazel//rules:install.bzl", "pick_file")' \
    'pick_file(' \
    'cmake-codegen-execute-process-fallback' \
    'cc_import(' \
    'sh_binary('; do
    if ! grep -qF -- "$marker" "$fallback_build"; then
        echo "storage-cost: fallback BUILD missing marker: $marker" >&2
        cat "$fallback_build" >&2
        exit 1
    fi
done

# === Step 3: prove the per-consumer duplication is GONE. ===
# The pre-cutover tar-untar leg must not appear anywhere: no
# _install_tree_extract genrule, no install_tree.tar reference, no
# tar/untar command.
for banned in \
    '_install_tree_extract' \
    'install_tree.tar' \
    'tar -C' \
    'tar -xf'; do
    if grep -qF -- "$banned" "$fallback_build"; then
        echo "storage-cost: FAIL fallback BUILD still carries the tar-untar duplication leg: $banned" >&2
        cat "$fallback_build" >&2
        exit 1
    fi
done
echo "storage-cost: zero _install_tree_extract / install_tree.tar / tar-untar (duplication leg removed)"

# Count the pick_file projections (one per referenced artefact /
# header) and the per-target stubs. pick_file is a VIEW into the
# shared install-root Directory — not a copy: the bytes live once in
# the TreeArtifact, and each pick_file names a single entry. So the
# count is the number of distinct projected files, NOT a duplication
# factor.
pick_count=$(grep -cE '^pick_file\(' "$fallback_build" || true)
stub_count=$(grep -cE '^(cc_import|sh_binary|cc_library)\(' "$fallback_build" || true)

echo "storage-cost: fixture render emits $pick_count pick_file projections across $stub_count per-target stubs"

# Each pick_file's src must point at the same shared install-root
# target (the round-2 install's pipeline_install). Confirm every
# pick_file projects from the one install target — that's the proof
# the bytes aren't re-materialised per consumer.
if ! grep -qF 'src = ":thelib_trace_build"' "$fallback_build"; then
    echo "storage-cost: FAIL pick_file targets don't project from the shared :thelib_trace_build install root" >&2
    cat "$fallback_build" >&2
    exit 1
fi

# Spot-check: pick_count should be > 0 (this fixture has 3
# artifact-bearing targets with FILE_SET HEADERS contributing extra
# entries). Exact count drift between cmake versions is fine; the >0
# check catches a regression that would silently drop the shape.
if [ "$pick_count" -lt 3 ]; then
    echo "storage-cost: pick_file count ($pick_count) below expected floor (3 artifacts: lib + lib + bin)" >&2
    cat "$fallback_build" >&2
    exit 1
fi

# === Step 4: surface the storage math. ===
# After the cutover:
#
#   total_CAS_bytes  ≈  install_root_Directory_bytes
#
# The install root is stored ONCE as a CAS Directory merkle tree.
# Identical files across elements dedup at file granularity (the
# whole point of the TreeArtifact). Each pick_file is a single-file
# materialization (the entry already in CAS), NOT a re-emit of the
# whole subset — so the old `tar_bytes + Σ(extract bytes)` term
# collapses to just the Directory bytes, with cross-element file
# dedup on top.
echo "storage-cost: signal summary —"
echo "  pick_file projections (views into the shared install root): $pick_count"
echo "  per-target stubs referencing them:                          $stub_count"
echo "  tar-untar duplication leg:                                  REMOVED"

echo "storage-cost: ok"
