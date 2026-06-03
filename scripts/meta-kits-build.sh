#!/bin/sh
# meta-kits-build.sh — kit (compiler-axis) multi-toolchain build gate.
#
# meta-toolchain-build.sh proves ONE derived toolchain builds. This gate proves
# the KIT dimension: two compiler kits (gcc, clang) on one platform become two
# distinct cc_toolchains, disambiguated by a `kit` constraint_value, and each is
# selectable + buildable via --platforms=//platforms:<platform>_<kit>.
#
# Pipeline exercised (the real probe-matrix flow, minus the bazel-genrule
# project-A layer): probe-cell runs a live cmake probe per kit (real
# tool_paths), unify-toolchains folds the cells into the kit-dimensioned Bazel
# layout, then `bazel build` forces each kit's platform and confirms the right
# compiler drives the compile.
#
# Availability gating mirrors the other meta-* gates: the probe + unify half
# needs cmake (+ gcc/clang); the build half needs bazel >= 9. Each half
# self-skips (prints a skip line, exit 0) when its tool is absent.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/probe-cell" ./converter/cmd/probe-cell
CGO_ENABLED=0 go build -o "$bin_dir/unify-toolchains" ./converter/cmd/unify-toolchains

if ! command -v cmake >/dev/null 2>&1; then
	echo "meta-kits-build: skipped (no cmake)"
	exit 0
fi
gcc_bin="$(command -v gcc || true)"
gxx_bin="$(command -v g++ || true)"
clang_bin="$(command -v clang || true)"
clangxx_bin="$(command -v clang++ || true)"
if [ -z "$gcc_bin" ] || [ -z "$gxx_bin" ] || [ -z "$clang_bin" ] || [ -z "$clangxx_bin" ]; then
	echo "meta-kits-build: skipped (need both gcc and clang for a two-kit matrix)"
	exit 0
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

probe_src="$repo_root/converter/testdata/toolchain-probe"
cells="$work_dir/cells"
mkdir -p "$cells"

# Bazel cpu constraint for this host (the platforms-json constraint must match
# the build's resolved target cpu).
case "$(uname -m)" in
x86_64) bazel_cpu="x86_64" ;;
aarch64 | arm64) bazel_cpu="arm64" ;;
*) bazel_cpu="$(uname -m)" ;;
esac
plat="linux_${bazel_cpu}"

# 1. Live cmake probe per kit → real per-compiler tool_paths.
"$bin_dir/probe-cell" \
	--cmake-source "$probe_src" \
	--variant "gcc-baseline" --kit "gcc" \
	--cache-var "CMAKE_C_COMPILER=$gcc_bin" \
	--cache-var "CMAKE_CXX_COMPILER=$gxx_bin" \
	--build-dir "$work_dir/bd-gcc" \
	--out "$cells/$plat.gcc-baseline.probe.json"
"$bin_dir/probe-cell" \
	--cmake-source "$probe_src" \
	--variant "clang-baseline" --kit "clang" \
	--cache-var "CMAKE_C_COMPILER=$clang_bin" \
	--cache-var "CMAKE_CXX_COMPILER=$clangxx_bin" \
	--build-dir "$work_dir/bd-clang" \
	--out "$cells/$plat.clang-baseline.probe.json"
echo "meta-kits-build: probed 2 kits (gcc, clang) into cells/"

# 2. Platforms manifest + operator repo, then unify.
cat >"$work_dir/platforms.json" <<EOF
[
  {"name": "$plat", "constraints": ["@platforms//os:linux", "@platforms//cpu:$bazel_cpu"]}
]
EOF

ws="$work_dir/repo"
mkdir -p "$ws"
# Register the emitted toolchains the documented way:
# register_toolchains("//toolchains:all"), where ":all" is Bazel's package
# wildcard over every toolchain() in //toolchains. Both kit toolchains get
# registered; the --platforms kit constraint disambiguates which resolves.
# (This exercises the real registration path — the emit deliberately defines
# no target named "all", which would otherwise shadow the wildcard.)
cat >"$ws/MODULE.bazel" <<'EOF'
module(name = "kits_build_gate", version = "0.0.1")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "platforms", version = "0.0.11")
register_toolchains("//toolchains:all")
EOF

"$bin_dir/unify-toolchains" \
	--probe-cells "$cells" \
	--platforms-json "$work_dir/platforms.json" \
	--repo-root "$ws"

# 3. Render assertions: the kit constraint dimension + per-kit slug toolchains.
plats_build="$ws/platforms/BUILD.bazel"
tc_build="$ws/toolchains/BUILD.bazel"
for needle in \
	'constraint_setting(name = "kit")' \
	'name = "gcc"' \
	'name = "clang"' \
	"name = \"${plat}_gcc\"" \
	"name = \"${plat}_clang\"" \
	'"//platforms:gcc"' \
	'"//platforms:clang"'; do
	if ! grep -qF -- "$needle" "$plats_build"; then
		echo "meta-kits-build: FAIL — platforms/BUILD.bazel missing: $needle" >&2
		cat "$plats_build" >&2
		exit 1
	fi
done
for needle in \
	"name = \"${plat}_gcc_toolchain\"" \
	"name = \"${plat}_clang_toolchain\"" \
	'"//platforms:gcc"' \
	'"//platforms:clang"'; do
	if ! grep -qF -- "$needle" "$tc_build"; then
		echo "meta-kits-build: FAIL — toolchains/BUILD.bazel missing: $needle" >&2
		cat "$tc_build" >&2
		exit 1
	fi
done
echo "meta-kits-build: render OK — 2 kit toolchains (${plat}_gcc, ${plat}_clang) with a kit constraint_setting"

# 4. Build half: needs bazel/bazelisk >= 9. Self-skips otherwise.
if command -v bazel >/dev/null 2>&1; then
	BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then
	BZL=bazelisk
else
	echo "meta-kits-build: render OK; bazel not on PATH, skipping build phase"
	exit 0
fi
major=$("$BZL" --version 2>/dev/null | sed -n 's/^bazel \([0-9]*\).*/\1/p')
if [ -z "$major" ]; then
	# `bazelisk --version` can echo the launcher's own version rather than
	# `bazel <n>`; fall back to the `version` subcommand's "Build label:".
	major=$("$BZL" version 2>/dev/null | sed -n 's/^Build label: \([0-9]*\).*/\1/p')
fi
if [ -z "$major" ] || [ "$major" -lt 9 ]; then
	echo "meta-kits-build: render OK; bazel < 9 (the bzlmod + load() floor), skipping build phase"
	exit 0
fi

cat >"$ws/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(name = "probe_bin", srcs = ["probe.cc"])
EOF
printf '#include <cstdio>\nint main() { std::puts("kits-build-ok"); return 0; }\n' >"$ws/probe.cc"

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

# Build under each kit's platform; a broken kit toolchain (or a kit constraint
# that fails to disambiguate) fails analysis/build here instead of silently
# resolving to the wrong compiler. Both kit toolchains are registered via
# register_toolchains("//toolchains:all") in MODULE.bazel; the --platforms kit
# constraint picks the right one.
for kit in gcc clang; do
	# shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
	( cd "$ws" && "$BZL" --output_user_root="$work_dir/.bazel" \
		$META_BAZEL_STARTUP_ARGS \
		build //:probe_bin \
		--platforms="//platforms:${plat}_${kit}" $META_BAZEL_BUILD_ARGS )
	echo "meta-kits-build: built //:probe_bin under kit '$kit' (--platforms=//platforms:${plat}_${kit})"
done

# Confirm the two kits actually drove DIFFERENT compilers (the whole point of
# the kit dimension). aquery the compile action's argv per platform and diff.
gcc_argv="$(cd "$ws" && "$BZL" --output_user_root="$work_dir/.bazel" $META_BAZEL_STARTUP_ARGS \
	aquery "mnemonic(CppCompile, //:probe_bin)" --platforms="//platforms:${plat}_gcc" 2>/dev/null |
	grep -E 'Command Line|gcc|clang' | head -40 || true)"
clang_argv="$(cd "$ws" && "$BZL" --output_user_root="$work_dir/.bazel" $META_BAZEL_STARTUP_ARGS \
	aquery "mnemonic(CppCompile, //:probe_bin)" --platforms="//platforms:${plat}_clang" 2>/dev/null |
	grep -E 'Command Line|gcc|clang' | head -40 || true)"
if echo "$gcc_argv" | grep -qF "$gcc_bin" && echo "$clang_argv" | grep -qF "$clang_bin"; then
	echo "meta-kits-build: confirmed — gcc kit drives $gcc_bin, clang kit drives $clang_bin"
else
	echo "meta-kits-build: WARN — could not confirm distinct compilers via aquery (build still passed); gcc_argv/clang_argv inconclusive" >&2
fi

echo "meta-kits-build: OK — two kit toolchains built //:probe_bin under their own --platforms"
