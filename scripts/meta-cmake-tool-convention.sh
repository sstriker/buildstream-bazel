#!/bin/sh
# meta-cmake-tool-convention.sh — render gate for the built-in tool→label
# convention registry (auto-DERIVING a host codegen tool's label).
#
# The fixture's add_custom_command drives a WELL-KNOWN host tool (protoc),
# recovered as a genrule (--recognize-codegen OFF, so it stays a genrule rather
# than a proto_library — the convention's domain). Three behaviors:
#   1. Default: the genrule keeps the raw `protoc` driver, and the
#      host-codegen-tool conversion-todo names the CANONICAL convention label
#      (@protobuf//:protoc) + the bazel_dep to add — no placeholder.
#   2. --tool-conventions: the built-in registry auto-hermeticizes the driver to
#      $(execpath @protobuf//:protoc) through the tool-swap, and the todo is
#      suppressed (it's hermetic now).
#
# Render-only: the swapped label (@protobuf) isn't fetchable offline; the
# bazel-build half of the tool-swap is already covered by
# meta-cmake-host-codegen-tool.sh.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/tool-convention"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Default: raw driver + convention-aware todo (no auto-swap). ---
c="$work_dir/c"; mkdir -p "$c"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$c/BUILD.bazel" --conversion-todos-report "$c/todos.json" \
    >"$c/out" 2>"$c/err" || fail "default convert failed" "$c/err"
grep -qF 'cmd = "protoc --plain_c' "$c/BUILD.bazel" \
    || fail "default should keep the raw protoc driver (no auto-swap)" "$c/BUILD.bazel"
grep -qF '"convention_label": "@protobuf//:protoc"' "$c/todos.json" \
    || fail "the todo should name the canonical convention label for protoc" "$c/todos.json"
grep -qF 'bazel_dep(name = \"protobuf\"' "$c/todos.json" \
    || fail "the todo should name the bazel_dep to add for the convention" "$c/todos.json"
echo "ok  meta-cmake-tool-convention: default keeps the raw driver + suggests the canonical label"

# --- --tool-conventions: auto-hermeticize the known tool, suppress the todo. ---
t="$work_dir/t"; mkdir -p "$t"
"$bin_dir/convert-element-cmake" --source-root "$fixture" --tool-conventions \
    --out-build "$t/BUILD.bazel" --conversion-todos-report "$t/todos.json" \
    >"$t/out" 2>"$t/err" || fail "--tool-conventions convert failed" "$t/err"
grep -qF '$(execpath @protobuf//:protoc) --plain_c' "$t/BUILD.bazel" \
    || fail "--tool-conventions should swap protoc to \$(execpath @protobuf//:protoc)" "$t/BUILD.bazel"
grep -qF 'tools = ["@protobuf//:protoc"]' "$t/BUILD.bazel" \
    || fail "the swapped genrule should carry the convention label in tools" "$t/BUILD.bazel"
grep -qF '"kind": "host-codegen-tool"' "$t/todos.json" \
    && fail "an auto-hermeticized tool must NOT surface a host-codegen-tool todo" "$t/todos.json"
echo "ok  meta-cmake-tool-convention: --tool-conventions auto-hermeticizes the known tool (todo suppressed)"
echo "ok  meta-cmake-tool-convention: ok"
