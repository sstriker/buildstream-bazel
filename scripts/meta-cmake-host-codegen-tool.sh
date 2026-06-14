#!/bin/sh
# meta-cmake-host-codegen-tool.sh — render+build gate for the imports-manifest
# `tools` map: hermeticizing a host CODEGEN tool that has NO native Bazel rule.
#
# The fixture's add_custom_command drives a project-specific host tool
# (gen.sh) by absolute source path to generate greeting.c, consumed by a
# cc_library. Without a manifest the recovered genrule keeps the raw host tool
# reference (`gen.sh ...`) — non-hermetic, relying on whatever gen.sh resolves
# to in the sandbox. With an imports.json carrying a `tools` map
# (`{"match":"gen.sh","label":"//:gen_tool"}`) the single tool-swap chokepoint
# (rewriteToolFromTarget) rewrites the driver to `$(execpath //:gen_tool)` and
# adds the label to the genrule's `tools`, so Bazel stages and runs the
# hermetic tool.
#
# Control asserts the raw host-tool reference; the manifest asserts the swap;
# then bazel builds the consuming library (proving the swapped tool runs).
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/host-codegen-tool"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: no manifest → raw host-tool reference, no execpath swap. ---
c="$work_dir/c"; mkdir -p "$c"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$c/BUILD.bazel" >"$c/out" 2>"$c/err" || fail "control convert failed" "$c/err"
grep -qF 'cmd = "gen.sh greeting.in' "$c/BUILD.bazel" \
    || fail "control should keep the raw host-tool reference (gen.sh)" "$c/BUILD.bazel"
grep -qF 'execpath' "$c/BUILD.bazel" \
    && fail "control must not swap to an execpath label without a manifest" "$c/BUILD.bazel"
echo "ok  meta-cmake-host-codegen-tool: control keeps the raw host-tool reference"

# --- Manifest: the `tools` map drives the hermetic swap. ---
m="$work_dir/m"; mkdir -p "$m"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --imports-manifest "$fixture/imports.json" \
    --out-build "$m/BUILD.bazel" >"$m/out" 2>"$m/err" || fail "manifest convert failed" "$m/err"
b="$m/BUILD.bazel"
grep -qF '$(execpath //:gen_tool) greeting.in $(RULEDIR)/greeting.c' "$b" \
    || fail "the genrule should drive the hermetic tool via \$(execpath //:gen_tool)" "$b"
grep -qF 'tools = ["//:gen_tool"]' "$b" \
    || fail "the genrule should carry the swapped label in tools" "$b"
echo "ok  meta-cmake-host-codegen-tool: the tools map swaps the host tool to \$(execpath)"

# --- Bazel-build half: prove the swapped tool actually runs. ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-host-codegen-tool: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-host-codegen-tool: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/gen.sh" "$fixture/greeting.in" "$ws/"
cp "$b" "$ws/BUILD.bazel"
# Provide the //:gen_tool label the manifest mapped the host tool to — a
# filegroup over the (executable) script the genrule's $(execpath) resolves.
cat >> "$ws/BUILD.bazel" <<'EOF'

filegroup(
    name = "gen_tool",
    srcs = ["gen.sh"],
)
EOF
cat > "$ws/MODULE.bazel" <<EOF
module(name = "hostcodegentool", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:greeting ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:greeting failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-host-codegen-tool: //:greeting builds from the hermetic-tool genrule"
