#!/bin/sh
# meta-unify-toolchains.sh — Stage 5 acceptance gate.
#
# Drives unify-toolchains end-to-end against synthetic per-cell probe
# artifacts (built from the recorded hello-world fileapi reply).
# Asserts:
#
#   - All four tool-owned files land at the expected paths.
#   - .bazelrc carries the try-import + sanitizer aliases + platform
#     aliases.
#   - cc_toolchain_config.bzl is one attr-driven rule (not the Stage 2
#     module-constants shape).
#   - toolchains/BUILD.bazel has per-platform cc_toolchain_config +
#     cc_toolchain + toolchain() trios plus the aggregating
#     filegroup `all`.
#   - MODULE.bazel is NOT touched.
#   - The first-run setup banner appears when MODULE.bazel lacks
#     `register_toolchains("//toolchains:all")`.
#
# Render-only (no bazel build); the rendered contract is what
# downstream stages consume.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/unify-toolchains" ./converter/cmd/unify-toolchains

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

# Build the in-repo fixture generator (probe-cell-fixture) and use
# it to mint probe.json artifacts for our matrix.
CGO_ENABLED=0 go build -o "$bin_dir/probe-cell-fixture" ./converter/cmd/probe-cell-fixture
cells_dir="$work_dir/cells"
"$bin_dir/probe-cell-fixture" \
    --fileapi-fixture "$repo_root/converter/testdata/fileapi/hello-world" \
    --out-dir "$cells_dir" \
    --cell "linux_x86_64:baseline" \
    --cell "linux_x86_64:debug:CMAKE_BUILD_TYPE=Debug" \
    --cell "linux_x86_64:release:CMAKE_BUILD_TYPE=Release" \
    --cell "linux_aarch64:baseline" \
    --cell "linux_aarch64:debug:CMAKE_BUILD_TYPE=Debug" \
    --cell "linux_aarch64:release:CMAKE_BUILD_TYPE=Release"

# Platforms manifest mirrors render-project-a's input shape.
cat > "$work_dir/platforms.json" <<'EOF'
[
  {"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
  {"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
]
EOF

# Operator's repo root: a minimal MODULE.bazel without register_toolchains
# so we can verify the setup banner fires.
operator_repo="$work_dir/operator-repo"
mkdir -p "$operator_repo"
cat > "$operator_repo/MODULE.bazel" <<'EOF'
module(name = "operator", version = "0.1.0")
EOF

stderr_log="$work_dir/unify.stderr"
"$bin_dir/unify-toolchains" \
    --probe-cells "$cells_dir" \
    --platforms-json "$work_dir/platforms.json" \
    --repo-root "$operator_repo" \
    2> "$stderr_log"

# 1. The four tool-owned files exist.
for rel in \
    platforms/BUILD.bazel \
    toolchains/BUILD.bazel \
    toolchains/cc_toolchain_config.bzl \
    .bazelrc \
; do
    if [ ! -f "$operator_repo/$rel" ]; then
        echo "missing tool-owned file: $rel" >&2
        exit 1
    fi
done

# 2. .bazelrc shape.
required_rc=$(cat <<'EOF'
try-import %workspace%/user.bazelrc
build:asan --features=asan
build:tsan --features=tsan
build:msan --features=msan
build:ubsan --features=ubsan
build:coverage --features=coverage
build:lto --features=lto
build:linux_x86_64 --platforms=//platforms:linux_x86_64
build:linux_aarch64 --platforms=//platforms:linux_aarch64
EOF
)
while IFS= read -r line; do
    if [ -z "$line" ]; then continue; fi
    if ! grep -qF -- "$line" "$operator_repo/.bazelrc"; then
        echo ".bazelrc missing line: $line" >&2
        cat "$operator_repo/.bazelrc" >&2
        exit 1
    fi
done <<EOF
$required_rc
EOF

# 3. cc_toolchain_config.bzl is the attr-driven shape.
required_cfg=$(cat <<'EOF'
def _impl(ctx):
cc_toolchain_config = rule(
"cpu": attr.string(mandatory = True)
"asan_compile_flags": attr.string_list(default = [])
provides = [CcToolchainConfigInfo]
EOF
)
while IFS= read -r line; do
    if [ -z "$line" ]; then continue; fi
    if ! grep -qF -- "$line" "$operator_repo/toolchains/cc_toolchain_config.bzl"; then
        echo "cc_toolchain_config.bzl missing: $line" >&2
        exit 1
    fi
done <<EOF
$required_cfg
EOF

# 4. toolchains/BUILD.bazel has the per-platform trios + filegroup all.
required_tc=$(cat <<'EOF'
name = "linux_x86_64_config"
name = "linux_x86_64_toolchain"
name = "linux_aarch64_toolchain"
name = "all"
":linux_x86_64_toolchain"
":linux_aarch64_toolchain"
EOF
)
while IFS= read -r line; do
    if [ -z "$line" ]; then continue; fi
    if ! grep -qF -- "$line" "$operator_repo/toolchains/BUILD.bazel"; then
        echo "toolchains/BUILD.bazel missing: $line" >&2
        exit 1
    fi
done <<EOF
$required_tc
EOF

# 5. MODULE.bazel was NOT modified.
if ! grep -qE '^module\(name = "operator"' "$operator_repo/MODULE.bazel"; then
    echo "MODULE.bazel content was modified" >&2
    cat "$operator_repo/MODULE.bazel" >&2
    exit 1
fi
if grep -q "register_toolchains" "$operator_repo/MODULE.bazel"; then
    echo "MODULE.bazel was edited (it should not be)" >&2
    cat "$operator_repo/MODULE.bazel" >&2
    exit 1
fi

# 6. The setup banner appeared in stderr.
if ! grep -qF "ONE-TIME SETUP" "$stderr_log"; then
    echo "expected first-run setup banner missing from stderr" >&2
    cat "$stderr_log" >&2
    exit 1
fi
if ! grep -qF 'register_toolchains("//toolchains:all")' "$stderr_log"; then
    echo "setup banner missing register_toolchains hint" >&2
    cat "$stderr_log" >&2
    exit 1
fi

# 7. Determinism: re-run, assert all four files byte-stable.
operator_repo2="$work_dir/operator-repo-2"
mkdir -p "$operator_repo2"
cat > "$operator_repo2/MODULE.bazel" <<'EOF'
module(name = "operator", version = "0.1.0")
EOF
"$bin_dir/unify-toolchains" \
    --probe-cells "$cells_dir" \
    --platforms-json "$work_dir/platforms.json" \
    --repo-root "$operator_repo2" \
    2> /dev/null
for rel in \
    platforms/BUILD.bazel \
    toolchains/BUILD.bazel \
    toolchains/cc_toolchain_config.bzl \
    .bazelrc \
; do
    if ! diff -q "$operator_repo/$rel" "$operator_repo2/$rel" >/dev/null; then
        echo "$rel diverged across runs (non-deterministic)" >&2
        diff -u "$operator_repo/$rel" "$operator_repo2/$rel" >&2
        exit 1
    fi
done

# 8. Second-run banner does NOT print when MODULE.bazel HAS the
# register_toolchains line.
echo 'register_toolchains("//toolchains:all")' >> "$operator_repo/MODULE.bazel"
stderr_log2="$work_dir/unify.stderr.run2"
"$bin_dir/unify-toolchains" \
    --probe-cells "$cells_dir" \
    --platforms-json "$work_dir/platforms.json" \
    --repo-root "$operator_repo" \
    2> "$stderr_log2"
if grep -qF "ONE-TIME SETUP" "$stderr_log2"; then
    echo "setup banner re-printed even after MODULE.bazel was patched" >&2
    cat "$stderr_log2" >&2
    exit 1
fi

echo "meta-unify-toolchains: ok (4 files, 2 platforms, deterministic, banner gating works)"
