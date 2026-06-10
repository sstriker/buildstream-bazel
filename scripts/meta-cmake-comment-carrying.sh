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
#   4. a macro-declared target carries the comment above the macro INVOCATION
#      (its user-level call site), not the macro body's internal comment —
#      while targets declared directly in an include()d file keep their own
#      comments (an inclusion is a scope change, not a call site);
#   5. buildifier -mode=diff is a no-op over the emitted BUILD (the comments sit
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

# (1)-(4) Convert WITH comment-carrying.
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
# (4) Macro-declared target: the comment above the INVOCATION carries (leading
# + trailing), the macro body's internal comment does not — and the provenance
# breadcrumb leads with the invocation (`# Source:`) while keeping the
# macro-internal add_library on a `# Declared:` line.
assert_present "The gadget lib — declared via the helper macro." "the macro call-site leading comment"
assert_present "macro-made gadget" "the macro call-site trailing comment"
assert_breadcrumb() { # regex description
  if ! grep -qE -- "$1" "$ws/BUILD.bazel"; then
    echo "FAIL: expected $2 in the emitted BUILD: $1"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$ws/BUILD.bazel"
    exit 1
  fi
}
assert_breadcrumb '^# Source: CMakeLists\.txt:[0-9]+ \(add_gadget_lib\)' "the call-site # Source: breadcrumb"
assert_breadcrumb '^# Declared: CMakeLists\.txt:[0-9]+ \(add_library\)' "the macro-internal # Declared: breadcrumb"
# (4b) include()d-file declarations: an inclusion is a scope change, not an
# invocation — each target declared at the included file's top level keeps the
# comment above its OWN add_library (two targets, one included file: the
# shared-site guard must NOT fire), and no breadcrumb names the include() line.
assert_present "The alpha lib — declared at the top of an included file." "the first included-file leading comment"
assert_present "The beta lib — second target in the same included file." "the second included-file leading comment"
# (4c) Trace-synthesized INTERFACE lib via a helper FUNCTION (the abseil
# absl_cc_library shape — codemodel drops INTERFACE libs, so this rides the
# trace-synth lift): the invocation's leading + trailing comments carry, the
# helper body's internal comment does not, and the breadcrumb leads with the
# invocation while keeping the helper-internal add_library on # Declared:.
assert_present "The gizmo interface lib — declared via the helper function." "the trace-synth interface lib leading comment"
assert_present "trace-synth gizmo" "the trace-synth interface lib trailing comment"
assert_breadcrumb '^# Source: CMakeLists\.txt:[0-9]+ \(add_iface_lib\)' "the interface lib call-site # Source: breadcrumb"
assert_breadcrumb '^# Declared: cmake/iface_helpers\.cmake:[0-9]+ \(add_library\)' "the interface lib helper-internal # Declared: breadcrumb"
if grep -qF -- "inside the helper body" "$ws/BUILD.bazel"; then
  echo "FAIL: helper BODY comment misattributed to a target: inside the helper body"
  echo "   --- generated BUILD ---"
  sed 's/^/   /' "$ws/BUILD.bazel"
  exit 1
fi
# (4d) Macro-wrapped codegen genrule: the comment above the gen_lut(...)
# invocation carries to the synthesized genrule (leading + trailing), the
# macro body's internal comment does not, and the genrule's breadcrumb leads
# with the invocation while keeping the add_custom_command on # Declared:.
assert_present "Generate the LUT — wrapped in the codegen macro." "the macro-wrapped genrule leading comment"
assert_present "macro-made lut" "the macro-wrapped genrule trailing comment"
assert_breadcrumb '^# Source: CMakeLists\.txt:[0-9]+ \(gen_lut\)' "the genrule call-site # Source: breadcrumb"
assert_breadcrumb '^# Declared: CMakeLists\.txt:[0-9]+ \(add_custom_command\)' "the genrule macro-internal # Declared: breadcrumb"
if grep -qF -- "inside the codegen macro" "$ws/BUILD.bazel"; then
  echo "FAIL: codegen macro BODY comment misattributed to a genrule: inside the codegen macro"
  echo "   --- generated BUILD ---"
  sed 's/^/   /' "$ws/BUILD.bazel"
  exit 1
fi
if grep -qE -- '^# Source: [^ ]+ \(include\)' "$ws/BUILD.bazel"; then
  echo "FAIL: an include() line leads a # Source: breadcrumb (inclusions are not call sites)"
  echo "   --- generated BUILD ---"
  sed 's/^/   /' "$ws/BUILD.bazel"
  exit 1
fi
if grep -qF -- "inside the macro body" "$ws/BUILD.bazel"; then
  echo "FAIL: macro BODY comment misattributed to a target: inside the macro body"
  echo "   --- generated BUILD ---"
  sed 's/^/   /' "$ws/BUILD.bazel"
  exit 1
fi
echo "ok  meta-cmake-comment-carrying: file header + target (leading+trailing) + codegen + macro call-site comments carried"

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
# Assert EVERY carried-comment class is suppressed — the same four markers the
# positive check asserts present (file-header, target leading, codegen leading,
# target trailing) — so a partial regression (e.g. only the header leaks) can't
# slip through.
for marker in \
  "Copyright 2026 the comment-carrying authors." \
  "wraps the vendored widget code" \
  "Generate the lookup table from the spec" \
  "the widget core lib" \
  "The gadget lib — declared via the helper macro." \
  "macro-made gadget" \
  "The alpha lib — declared at the top of an included file." \
  "The beta lib — second target in the same included file." \
  "The gizmo interface lib — declared via the helper function." \
  "trace-synth gizmo" \
  "Generate the LUT — wrapped in the codegen macro." \
  "macro-made lut"; do
  if grep -qF "$marker" "$ws/BUILD.nocomments"; then
    echo "FAIL: author comment present with --emit-source-comments=false: $marker"
    exit 1
  fi
done
echo "ok  meta-cmake-comment-carrying: --emit-source-comments=false suppresses all author comments"

# (5) buildifier -mode=diff must be a no-op (canonical comment placement).
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
