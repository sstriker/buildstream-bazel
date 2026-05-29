#!/bin/sh
# meta-pyproject-fallback.sh — render-half acceptance gate for
# the kind:pyproject Phase B install-plan fallback (option A:
# per-element auto-detection). Drives write-a against TWO
# kind:pyproject fixtures with --convert-element-pyproject +
# --pyproject-fallback set:
#
#   1. pyproject-greet (setuptools, native-friendly) — should
#      render as the converter genrule (// tools:convert-element-
#      pyproject invocation).
#   2. pyproject-pdm-greet (pdm.backend, refused by Phase A) —
#      should fall back to the pipeline shape (the coarse
#      install_tree.tar genrule), because the probe rejects
#      pdm-backend.
#
# The render-half checks are the only assertions today (no
# bazel-build half) — the fallback's value is in dispatch
# correctness at write-a time. The bazel-build half is exercised
# by meta-pyproject.sh for the native path and by the existing
# pipeline-shape gates for the coarse path.
#
# Bazel-availability gating: this gate doesn't drive bazel; the
# render-half checks always run.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-pyproject" ./converter/cmd/convert-element-pyproject

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

# Two-element invocation: one Phase-A-friendly + one refused.
"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/pyproject-greet.bst \
    --bst testdata/meta-project/pyproject-pdm-greet.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-pyproject "$bin_dir/convert-element-pyproject" \
    --pyproject-fallback 2>"$work_dir/write-a.stderr"

# Phase-A-friendly element renders as the converter genrule.
greet_build="$A/elements/pyproject-greet/BUILD.bazel"
if ! grep -qF -- '//tools:convert-element-pyproject' "$greet_build"; then
    echo "meta-pyproject-fallback: pyproject-greet should have rendered the native genrule (probe says it's Phase-A-friendly)" >&2
    cat "$greet_build" >&2
    exit 1
fi
echo "meta-pyproject-fallback: pyproject-greet rendered native genrule (Phase A succeeds)"

# Refused element renders as the pipeline shape (coarse install
# genrule producing install_tree.tar).
pdm_build="$A/elements/pyproject-pdm-greet/BUILD.bazel"
if grep -qF -- '//tools:convert-element-pyproject' "$pdm_build"; then
    echo "meta-pyproject-fallback: pyproject-pdm-greet should have fallen back to pipeline shape (probe refuses pdm.backend)" >&2
    cat "$pdm_build" >&2
    exit 1
fi
if ! grep -qF -- 'pipeline_install(' "$pdm_build"; then
    echo "meta-pyproject-fallback: pyproject-pdm-greet's fallback BUILD missing pipeline_install (expected pipeline shape)" >&2
    cat "$pdm_build" >&2
    exit 1
fi
echo "meta-pyproject-fallback: pyproject-pdm-greet fell back to pipeline shape (Phase A refuses)"

# Diagnostic: the per-element refusal reason should be on
# write-a's stderr so operators see WHY the fallback happened.
if ! grep -qF 'pyproject-pdm-greet: probe refuses' "$work_dir/write-a.stderr"; then
    echo "meta-pyproject-fallback: missing operator-facing refusal diagnostic on stderr" >&2
    cat "$work_dir/write-a.stderr" >&2
    exit 1
fi
echo "meta-pyproject-fallback: refusal diagnostic surfaced on stderr"

echo "meta-pyproject-fallback: ok (per-element auto-detection routes natively-renderable elements through Phase A and refuses-by-Phase-A elements through the pipeline shape)"
