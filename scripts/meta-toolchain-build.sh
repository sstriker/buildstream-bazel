#!/bin/sh
# meta-toolchain-build.sh — toolchain BUILD gate.
#
# The render gates (meta-unify-toolchains.sh) assert the *text* of the
# generated cc_toolchain layout but never compile with it. This gate closes
# that loop: derive a toolchain from a live cmake probe of this host, then
# `bazel build` a real C++ target *forcing that toolchain*. It is the
# regression test for the bazel-9 Starlark-autoload breakages a render-only
# check can't catch — the native cc_toolchain rule was removed, and cc_common
# / CcToolchainConfigInfo are no longer built-in globals, so the emitted
# cc_toolchain_config.bzl + toolchains/BUILD.bazel must carry explicit
# load()s. A regression there fails analysis here instead of silently
# shipping a toolchain that no operator can build with.
#
# Availability gating: the derive half needs cmake; the build half needs
# bazel. Each half self-skips (prints a skip line, exit 0) when its tool is
# absent, mirroring the other render gates.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/derive-toolchain" ./converter/cmd/derive-toolchain

if ! command -v cmake >/dev/null 2>&1; then
	echo "meta-toolchain-build: skipped (no cmake)"
	exit 0
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

ws="$work_dir/repo"
tc_dir="$ws/toolchains"
mkdir -p "$tc_dir"

# 1. Derive a host-accurate toolchain (live cmake probe → real tool_paths).
"$bin_dir/derive-toolchain" \
	--probe "$repo_root/converter/testdata/toolchain-probe" \
	--build-root "$work_dir/bd" \
	--out "$tc_dir"
echo "meta-toolchain-build: derived toolchain into toolchains/ ($(ls "$tc_dir" | tr '\n' ' '))"

# 2. Discover the emitted toolchain() target name.
tc_name="$(grep -oE 'name = "[A-Za-z0-9_]+_toolchain"' "$tc_dir/BUILD.bazel" |
	head -1 | sed -E 's/.*"([^"]+)".*/\1/')"
if [ -z "$tc_name" ]; then
	echo "meta-toolchain-build: FAIL — no toolchain() target in generated BUILD.bazel" >&2
	exit 1
fi
echo "meta-toolchain-build: toolchain target = //toolchains:$tc_name"

# 3. Build half: needs bazel/bazelisk >= 9 (the bzlmod + load() floor). Self-
#    skips otherwise, mirroring the other meta-* build halves.
if command -v bazel >/dev/null 2>&1; then
	BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then
	BZL=bazelisk
else
	echo "meta-toolchain-build: derive OK; bazel not on PATH, skipping build phase"
	exit 0
fi
major=$("$BZL" --version 2>/dev/null | sed -n 's/^bazel \([0-9]*\).*/\1/p')
if [ -z "$major" ] || [ "$major" -lt 9 ]; then
	echo "meta-toolchain-build: derive OK; bazel < 9 (the bzlmod + load() floor), skipping build phase"
	exit 0
fi

# 4. Minimal workspace that builds a C++ target with the generated toolchain.
cat >"$ws/MODULE.bazel" <<'EOF'
module(name = "tc_build_gate", version = "0.0.1")
bazel_dep(name = "rules_cc", version = "0.0.17")
# The generated toolchain pins its platform/target_compatible_with to
# @platforms constraints, so the consuming module must depend on it directly.
bazel_dep(name = "platforms", version = "0.0.11")
EOF
cat >"$ws/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(name = "probe_bin", srcs = ["probe.cc"])
EOF
printf '#include <cstdio>\nint main() { std::puts("toolchain-build-ok"); return 0; }\n' >"$ws/probe.cc"

# 5. Force the generated toolchain so a broken one fails here instead of
#    silently falling back to bazel's autodetected host toolchain. Isolate the
#    output root + thread the standard META_BAZEL_* overrides (meta-* convention).
META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}
# shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
( cd "$ws" && "$BZL" --output_user_root="$work_dir/.bazel" \
	$META_BAZEL_STARTUP_ARGS \
	build //:probe_bin --extra_toolchains="//toolchains:$tc_name" $META_BAZEL_BUILD_ARGS )

echo "meta-toolchain-build: OK — built //:probe_bin with the generated toolchain (//toolchains:$tc_name)"
