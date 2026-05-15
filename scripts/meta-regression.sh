#!/bin/sh
# meta-regression.sh — run-vs-run regression gate for the write-a +
# Bazel path.
#
# Re-homed from the orchestrator's regression e2e
# (docs/design/orchestrator-absorption.md). The orchestrator produced
# a <out>/manifest/{converted,failures,determinism}.json per run and
# orchestrate-diff compared two of them; the write-a + Bazel path
# produces the same run-manifest shape via `cmd/run-manifest` walking
# a built project A.
#
# What this gate proves — two halves, both faithful re-homes:
#
#   1. No-drift invariant. A content edit to a source file that cmake
#      does NOT read at configure time (a .c file) must NOT shift the
#      converted BUILD.bazel.out. Render → build A → run-manifest →
#      edit prod/src/prod.c → render → build A → run-manifest →
#      orchestrate-diff reports zero fingerprint drift, exit 0.
#      (Re-homes the orchestrator's ContentEditUnderShadowDoesntDrift.)
#
#   2. Drift IS detected. A CMakeLists.txt change that genuinely
#      shifts the codemodel (a new compile definition) must surface as
#      fingerprint drift for that element — proving the gate isn't
#      vacuous.
#
# What is deliberately NOT re-homed: newly-failed detection. The
# orchestrator's regression model assumed *soft* Tier-1 failures (the
# run completed, failures.json recorded casualties). The write-a +
# Bazel path is *hard*-fail — a Tier-1 makes `bazel build` in project
# A fail outright, so a run that exists has no failed elements. See
# cmd/run-manifest's doc comment and docs/design/orchestrator-absorption.md.
#
# Bazel-availability gating + META_BAZEL_*_ARGS mirror
# scripts/meta-cross-cmake.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake
CGO_ENABLED=0 go build -o "$bin_dir/run-manifest" ./cmd/run-manifest
CGO_ENABLED=0 go build -o "$bin_dir/orchestrate-diff" ./cmd/orchestrate-diff

# Editable copy of the cross-cmake fixture — the gate mutates sources,
# so it must not touch the checked-in tree.
fixture="$work_dir/fixture"
cp -r testdata/meta-project/cross-cmake "$fixture"

A="$work_dir/A"

render() {
    "$bin_dir/write-a" \
        --rules-package-path "$repo_root/rules_buildstream_bazel" \
        --bst "$fixture/prod.bst" \
        --bst "$fixture/cons.bst" \
        --out "$A" \
        --convert-element-cmake "$bin_dir/convert-element-cmake"
}
render

# Render-phase check — always runs, doesn't gate on bazel.
for want in elements/prod/BUILD.bazel elements/cons/BUILD.bazel; do
    if [ ! -f "$A/$want" ]; then
        echo "meta-regression: missing rendered project A file: $want" >&2
        exit 1
    fi
done
echo "meta-regression: render OK"

# Bazel-availability gating.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-regression: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-regression: render OK; bazel $($BZL --version | head -1) is < 9, skipping build phase"
    exit 0
fi

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}
bzl_cache="$work_dir/.bazel"

build_a() {
    # shellcheck disable=SC2086
    ( cd "$A" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        build //elements/prod:prod_converted //elements/cons:cons_converted \
        $META_BAZEL_BUILD_ARGS ) 2>&1 | tail -5
}

# snapshot writes a run manifest for the current built project A.
snapshot() {
    "$bin_dir/run-manifest" --project-a "$A" --out "$1"
}

# === Run 1: baseline ===
build_a
snapshot "$work_dir/run1"

# === No-drift edit: append a comment to a .c file (NOT configure-read) ===
echo "// meta-regression no-drift edit $(date +%s)" >> "$fixture/sources/prod/src/prod.c"
render
build_a
snapshot "$work_dir/run2"

echo "=== diff run1 -> run2 (content edit outside cmake's read set) ==="
diff_out="$work_dir/diff-nodrift.txt"
if ! "$bin_dir/orchestrate-diff" --before "$work_dir/run1" --after "$work_dir/run2" \
        --format=text --out "$diff_out"; then
    echo "meta-regression: orchestrate-diff reported a regression on a no-op .c edit" >&2
    cat "$diff_out" >&2
    exit 1
fi
if grep -qiE "drift|drifted" "$diff_out"; then
    echo "meta-regression: FAIL — fingerprint drift on a content edit cmake never reads" >&2
    cat "$diff_out" >&2
    exit 1
fi
echo "meta-regression: no-drift invariant holds (prod.c content edit did not shift BUILD.bazel.out)"

# === Drift edit: a CMakeLists.txt change that genuinely shifts the codemodel ===
echo 'target_compile_definitions(prod PRIVATE META_REGRESSION_DRIFT=1)' >> "$fixture/sources/prod/CMakeLists.txt"
render
build_a
snapshot "$work_dir/run3"

echo "=== diff run2 -> run3 (real codemodel change) ==="
diff_out3="$work_dir/diff-drift.json"
"$bin_dir/orchestrate-diff" --before "$work_dir/run2" --after "$work_dir/run3" \
    --format=json --out "$diff_out3" || true
if ! grep -q '"prod"' "$diff_out3" || ! grep -qiE "drift" "$diff_out3"; then
    echo "meta-regression: FAIL — orchestrate-diff did not report drift for prod after a real CMakeLists change" >&2
    cat "$diff_out3" >&2
    exit 1
fi
echo "meta-regression: drift detection works (CMakeLists change surfaced as fingerprint drift for prod)"

echo "meta-regression: ok (run-manifest + orchestrate-diff round-trip; no-drift invariant + drift detection both verified)"
