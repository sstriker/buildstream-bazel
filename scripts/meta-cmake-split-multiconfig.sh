#!/bin/sh
# meta-cmake-split-multiconfig.sh — end-to-end gate for --split-packages
# composed with multi-config (--build-types).
#
# The single-config split path is gated by meta-cmake-split-build.sh; this
# gate adds the multi-config layer: write-a --split-packages
# --build-types=auto must thread --build-types into the cmake_split_convert
# action (so each per-directory BUILD carries //config:<name> select() arms),
# render the //config package + bazel_skylib dep into project B, and the
# staged split tree must compile under --//config:build_type=debug with the
# selects resolving against that //config package.
#
# Same fixture (testdata/meta-project/split-cmake/) and Bazel-availability
# gating + META_BAZEL_*_ARGS overrides as meta-cmake-split-build.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work_dir=$(mktemp -d)
# TreeArtifact dirs are read-only; restore write perms before removing.
trap 'chmod -R u+w "$work_dir" 2>/dev/null || true; rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake
CGO_ENABLED=0 go build -o "$bin_dir/stage-b" ./cmd/stage-b

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/split-cmake/subdir-library.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --split-packages \
    --build-types=auto

# Render-phase checks: split rule present + multi-config wiring.
elem_build="$A/elements/subdir-library/BUILD.bazel"
if ! grep -q 'cmake_split_convert(' "$elem_build"; then
    echo "meta-cmake-split-multiconfig: element BUILD missing cmake_split_convert rule" >&2
    exit 1
fi
if ! grep -q -- '--build-types=auto' "$elem_build"; then
    echo "meta-cmake-split-multiconfig: split converter_args missing --build-types=auto" >&2
    cat "$elem_build" >&2
    exit 1
fi
# Project B carries the //config package + the skylib dep its string_flag needs.
if [ ! -f "$B/config/BUILD.bazel" ]; then
    echo "meta-cmake-split-multiconfig: project B missing //config/BUILD.bazel" >&2
    exit 1
fi
if ! grep -q 'bazel_skylib' "$B/MODULE.bazel"; then
    echo "meta-cmake-split-multiconfig: project B MODULE.bazel missing bazel_skylib dep" >&2
    exit 1
fi
echo "meta-cmake-split-multiconfig: render OK (split + multi-config + //config + skylib)"

# Bazel-availability gating (mirrors meta-cmake-split-build.sh).
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-cmake-split-multiconfig: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
major=$("$BZL" --version 2>/dev/null | sed -n 's/^bazel \([0-9]*\).*/\1/p')
if [ -z "$major" ] || [ "$major" -lt 9 ]; then
    echo "meta-cmake-split-multiconfig: render OK; bazel < 9 (the bzlmod + load() floor), skipping build phase"
    exit 0
fi
for tool in cmake ninja; do
    if ! command -v "$tool" >/dev/null; then
        echo "meta-cmake-split-multiconfig: render OK; $tool not on PATH, skipping build phase"
        exit 0
    fi
done

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}
bzl_cache="$work_dir/.bazel"

run_bazel() {
    workspace="$1"
    shift
    cmd="$1"
    shift
    # shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

# === Pass 1: build project A's split TreeArtifact (converter runs multi-config). ===
run_bazel "$A" build //elements/subdir-library:subdir-library_converted 2>&1 | tail -10
pkgs="$A/bazel-bin/elements/subdir-library/subdir-library_converted/packages"
# The per-directory BUILDs must carry the //config:<name> select() arms.
if ! grep -rq '//config:' "$pkgs"; then
    echo "meta-cmake-split-multiconfig: split BUILD tree carries no //config: select arms" >&2
    find "$pkgs" -name BUILD.bazel >&2 2>/dev/null || true
    exit 1
fi
echo "meta-cmake-split-multiconfig: split TreeArtifact carries //config: select arms"

# === Stage into B. ===
"$bin_dir/stage-b" --project-a "$A" --project-b "$B" >/dev/null

# === Pass 2: the split tree compiles under an explicit config flag. ===
# The select() arms must resolve against the rendered //config package.
run_bazel "$B" build \
    //elements/subdir-library:toplib \
    //elements/subdir-library/src/util:util \
    --//config:build_type=debug 2>&1 | tail -10

echo "meta-cmake-split-multiconfig: ok (split + multi-config render + TreeArtifact //config arms + stage-b + project B compiles under --//config:build_type=debug)"
