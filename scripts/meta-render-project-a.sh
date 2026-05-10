#!/bin/sh
# meta-render-project-a.sh — Stage 4 acceptance gate for the unified
# multi-platform toolchain plan.
#
# Drives render-project-a end-to-end against the canonical
# CMakePresets.json fixture (converter/testdata/toolchain-probe/) +
# a 2-platform manifest (linux_x86_64, linux_aarch64). Validates
# the rendered BUILD.bazel:
#
#   - One genrule per (variant, platform) cell.
#   - Each cell carries the platform's exec_compatible_with constraints.
#   - Each cell's cmd invokes probe-cell with the right --cache-var flags.
#   - The aggregating filegroup names every cell's output.
#
# This is a render-only gate: it doesn't try to bazel build the
# generated project A (that requires a real cmake source filegroup
# label, which only the operator's repo provides). The contract here
# is the rendering shape; downstream stages exercise the build.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/render-project-a" ./converter/cmd/render-project-a

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Build the platforms manifest.
cat > "$work_dir/platforms.json" <<EOF
[
  {"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
  {"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
]
EOF

out_dir="$work_dir/project-a"
"$bin_dir/render-project-a" \
    --out "$out_dir" \
    --variants-from "$repo_root/converter/testdata/toolchain-probe/CMakePresets.json" \
    --platforms-json "$work_dir/platforms.json"

build_bazel="$out_dir/BUILD.bazel"
if [ ! -f "$build_bazel" ]; then
    echo "render-project-a did not produce BUILD.bazel under $out_dir" >&2
    exit 1
fi

# Required content checks.
required_substrings=$(cat <<'EOF'
name = "linux_x86_64.baseline"
name = "linux_x86_64.asan"
name = "linux_x86_64.tsan"
name = "linux_x86_64.coverage"
name = "linux_aarch64.baseline"
name = "linux_aarch64.asan"
@platforms//cpu:x86_64
@platforms//cpu:arm64
exec_compatible_with = [
$(location //tools:probe-cell)
--variant 'asan'
--cache-var 'CMAKE_C_FLAGS=-fsanitize=address -fno-omit-frame-pointer'
name = "all_probes"
EOF
)

while IFS= read -r line; do
    if [ -z "$line" ]; then
        continue
    fi
    if ! grep -qF -- "$line" "$build_bazel"; then
        echo "BUILD.bazel missing required substring: $line" >&2
        echo "--- BUILD.bazel ---" >&2
        cat "$build_bazel" >&2
        exit 1
    fi
done <<EOF
$required_substrings
EOF

# Determinism: re-render and diff.
out_dir2="$work_dir/project-a-2"
"$bin_dir/render-project-a" \
    --out "$out_dir2" \
    --variants-from "$repo_root/converter/testdata/toolchain-probe/CMakePresets.json" \
    --platforms-json "$work_dir/platforms.json"

if ! diff -q "$out_dir/BUILD.bazel" "$out_dir2/BUILD.bazel" >/dev/null; then
    echo "render-project-a output not deterministic across runs" >&2
    diff -u "$out_dir/BUILD.bazel" "$out_dir2/BUILD.bazel" >&2
    exit 1
fi

echo "meta-render-project-a: ok ($(grep -c '^genrule(' "$build_bazel") cells across 2 platforms)"
