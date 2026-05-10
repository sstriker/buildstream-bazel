#!/bin/sh
# meta-element-fold.sh — render gate for the per-element
# multi-platform fold (Stages 1-6 of the follow-up plan to
# PR #105).
#
# Exercises fold-element directly against synthetic per-platform
# ir.Package JSONs that have intentional cross-platform
# divergence:
#
#   - Common src present in every cell (lands flat in baseline).
#   - Per-cell src present in only one cell (lands under
#     PerPlatform[srcs][selectKey]).
#   - Per-cell copts likewise.
#
# The rendered unified BUILD.bazel is asserted to contain the
# right select() shape — flat baseline plus a select() block
# with one arm per platform plus //conditions:default.
#
# Why direct-fold-element rather than full-orchestrator:
# spinning up a real cmake matrix (linux + darwin), the
# orchestrator's REAPI plumbing, and a worker pool for two
# platforms is materially more setup than this gate's cost
# budget. The fold-element entry point is the integration
# seam orchestrator/multiplatform.go consumes; exercising it
# directly catches regressions in elementfold + emit/bazel +
# the binary's CLI surface. A real-cmake end-to-end gate is
# tracked under ROADMAP follow-ups.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/fold-element" ./converter/cmd/fold-element

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Synthetic per-platform IRs. linux has src/foo_linux.c +
# common.c; darwin has src/foo_darwin.c + common.c. Copts
# diverge similarly. Booleans and identity strings agree so
# the fold doesn't error out on them.
cat > "$work_dir/linux.ir.json" <<'EOF'
{
  "Name": "libfoo",
  "SourceRoot": "/work/source",
  "Targets": [
    {
      "Name": "libfoo",
      "Kind": 1,
      "Srcs": ["common.c", "src/foo_linux.c"],
      "Hdrs": ["include/foo.h"],
      "Includes": ["include"],
      "Copts": ["-Wall", "-DLINUX"],
      "Defines": ["FOO_VERSION=1"],
      "Visibility": ["//visibility:public"]
    }
  ]
}
EOF

cat > "$work_dir/darwin.ir.json" <<'EOF'
{
  "Name": "libfoo",
  "SourceRoot": "/work/source",
  "Targets": [
    {
      "Name": "libfoo",
      "Kind": 1,
      "Srcs": ["common.c", "src/foo_darwin.c"],
      "Hdrs": ["include/foo.h"],
      "Includes": ["include"],
      "Copts": ["-Wall", "-DDARWIN"],
      "Defines": ["FOO_VERSION=1"],
      "Visibility": ["//visibility:public"]
    }
  ]
}
EOF

out_build="$work_dir/BUILD.bazel"
"$bin_dir/fold-element" \
    --out-build "$out_build" \
    --cell "linux|@platforms//os:linux,@platforms//cpu:x86_64|$work_dir/linux.ir.json" \
    --cell "darwin|@platforms//os:darwin,@platforms//cpu:arm64|$work_dir/darwin.ir.json"

if [ ! -f "$out_build" ]; then
    echo "fold-element did not produce $out_build" >&2
    exit 1
fi

# Required content checks. Each line below must appear in the
# rendered BUILD.bazel; grep -F (literal) keeps the script
# robust against regex meta-characters in the patterns.
required=$(cat <<'EOF'
cc_library(
    name = "libfoo",
"common.c"
"src/foo_darwin.c"
"src/foo_linux.c"
"-DDARWIN"
"-DLINUX"
select({
"@platforms//cpu:arm64"
"@platforms//cpu:x86_64"
"//conditions:default": [],
includes = ["include"]
hdrs = ["include/foo.h"]
defines = ["FOO_VERSION=1"]
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

# Determinism: re-run with the same inputs, diff outputs.
out_build_2="$work_dir/BUILD.bazel.2"
"$bin_dir/fold-element" \
    --out-build "$out_build_2" \
    --cell "linux|@platforms//os:linux,@platforms//cpu:x86_64|$work_dir/linux.ir.json" \
    --cell "darwin|@platforms//os:darwin,@platforms//cpu:arm64|$work_dir/darwin.ir.json"

if ! diff -q "$out_build" "$out_build_2" >/dev/null; then
    echo "fold-element output not deterministic across runs" >&2
    diff -u "$out_build" "$out_build_2" >&2
    exit 1
fi

# Single-cell N=1 degenerate case: emit must produce flat
# output (no select() blocks) byte-identical to today's single-
# platform shape. Run fold-element with one cell and assert no
# select() appears.
out_build_solo="$work_dir/BUILD.solo.bazel"
"$bin_dir/fold-element" \
    --out-build "$out_build_solo" \
    --cell "linux|@platforms//os:linux,@platforms//cpu:x86_64|$work_dir/linux.ir.json"

if grep -qF "select(" "$out_build_solo"; then
    echo "N=1 degenerate case should not produce select() blocks" >&2
    cat "$out_build_solo" >&2
    exit 1
fi
if ! grep -qF '"src/foo_linux.c"' "$out_build_solo"; then
    echo "N=1 case missing the linux src" >&2
    cat "$out_build_solo" >&2
    exit 1
fi

# Ambiguous-matrix rejection: {linux_x86_64, linux_aarch64,
# darwin_arm64} — linux_aarch64's constraints share os:linux
# with x86_64 and cpu:arm64 with darwin. PickSelectKeys errors;
# fold-element surfaces it.
cat > "$work_dir/aarch64.ir.json" <<'EOF'
{ "Name": "libfoo", "Targets": [{"Name": "libfoo", "Kind": 1, "Srcs": ["common.c"]}] }
EOF
if "$bin_dir/fold-element" \
    --out-build "$work_dir/BUILD.ambig.bazel" \
    --cell "linux_x86_64|@platforms//os:linux,@platforms//cpu:x86_64|$work_dir/linux.ir.json" \
    --cell "linux_aarch64|@platforms//os:linux,@platforms//cpu:arm64|$work_dir/aarch64.ir.json" \
    --cell "darwin_arm64|@platforms//os:darwin,@platforms//cpu:arm64|$work_dir/darwin.ir.json" \
    2>"$work_dir/ambig.stderr"; then
    echo "expected fold-element to reject ambiguous matrix" >&2
    exit 1
fi
if ! grep -qF "linux_aarch64" "$work_dir/ambig.stderr"; then
    echo "expected error to name the offending platform" >&2
    cat "$work_dir/ambig.stderr" >&2
    exit 1
fi

cells_in_unified=$(grep -c '"src/foo_linux.c"' "$out_build")
echo "meta-element-fold: ok (multi-platform fold + select() rendering + N=1 degenerate identity + ambiguous-matrix rejection)"
