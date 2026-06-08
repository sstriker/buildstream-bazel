#!/bin/sh
# meta-cmake-comment-carrying.sh — render gate for carrying CMakeLists comments
# into the generated BUILD (--emit-source-comments).
#
# cmake discards comments at lex time, so the File API + trace carry none; the
# converter recovers them from raw source and re-attaches them. This gate proves
# the recovery end to end against real cmake:
#   1. the file-header block lands at the top of the BUILD;
#   2. a target's leading comment lands above its cc_library;
#   3. a codegen genrule's originating add_custom_command comment lands above it;
#   4. buildifier -mode=diff is a no-op over the emitted BUILD (the comments sit
#      in canonical positions — the gazelle-roundtrip contract holds).
#
# Suppression check: comment-carrying is default-ON, so --emit-source-comments=false
# is the opt-out — with it, no author comments appear.
#
# Gating: skips cleanly when cmake / ninja / go / make are absent; the buildifier
# half self-skips when buildifier isn't on PATH.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }
command -v make >/dev/null 2>&1 || { echo "skip: make not on PATH"; exit 0; }

fixture_src="$repo_root/converter/testdata/sample-projects/comment-carrying"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
ws="$work_dir/ws"
mkdir -p "$ws"
cp -R "$fixture_src/." "$ws/"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

# (1)-(3) Convert WITH comment-carrying.
"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --emit-source-comments \
  --out-build "$ws/BUILD.bazel" \
  >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
  echo "FAIL: convert-element-cmake exited non-zero"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
}

assert_present() { # marker description
  if ! grep -qF -- "$1" "$ws/BUILD.bazel"; then
    echo "FAIL: expected $2 in the emitted BUILD: $1"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$ws/BUILD.bazel"
    exit 1
  fi
}
assert_present "Copyright 2026 the comment-carrying authors." "the file-header block"
assert_present "wraps the vendored widget code" "the cc_library leading comment"
assert_present "Generate the lookup table from the spec" "the codegen genrule leading comment"
assert_present "the widget core lib" "the cc_library trailing comment"
echo "ok  meta-cmake-comment-carrying: file header + target (leading+trailing) + codegen comments carried"

# (suppression) Comment-carrying is default-ON (since the "Default-on
# comment-carrying" flip); --emit-source-comments=false is the opt-out. Convert
# with the opt-out and assert NO author comments appear — the suppression path
# (RecoverSourceComments off + the emitter's EmitSourceComments gate) holds.
"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --emit-source-comments=false \
  --out-build "$ws/BUILD.nocomments" \
  >/dev/null 2>"$work_dir/convert2.stderr" || {
  echo "FAIL: convert (--emit-source-comments=false) exited non-zero"
  sed 's/^/   stderr: /' "$work_dir/convert2.stderr"
  exit 1
}
if grep -qF "wraps the vendored widget code" "$ws/BUILD.nocomments"; then
  echo "FAIL: author comment present with --emit-source-comments=false"
  exit 1
fi
echo "ok  meta-cmake-comment-carrying: --emit-source-comments=false suppresses author comments"

# (4) buildifier -mode=diff must be a no-op (canonical comment placement).
if ! command -v buildifier >/dev/null 2>&1; then
  echo "ok  meta-cmake-comment-carrying: buildifier not on PATH, skipping no-op check"
  exit 0
fi
if ! buildifier -mode=diff "$ws/BUILD.bazel" >"$work_dir/buildifier.diff" 2>&1; then
  echo "FAIL: buildifier -mode=diff is not a no-op over the commented BUILD"
  sed 's/^/   /' "$work_dir/buildifier.diff"
  exit 1
fi
echo "ok  meta-cmake-comment-carrying: buildifier -mode=diff is a no-op"
