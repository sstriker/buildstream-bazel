#!/bin/sh
# meta-cmake-derived-codegen-rerun.sh — render+build gate for the opt-in
# --lift-derived-codegen live upgrade of the derived-name stem-match bake.
#
# The fixture's execute_process runs a tool that DERIVES its output from the
# input stem and writes it to its cwd (the build dir): `python3 gen.py foo.in`
# -> foo.gen.cc. The output isn't an argv operand, so default recovery captures
# the configure-written bytes (a write_file bake). Under --lift-derived-codegen,
# the converter re-runs the tool as a live genrule (`cd $(RULEDIR)`, inputs made
# absolute so they survive the cd), so the output regenerates on input change.
#
# Control asserts the bake; the flag asserts the genrule re-run; then bazel
# builds the consuming library (proving the re-run actually produces the source).
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH (the fixture's codegen tool)"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/derived-codegen-rerun"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: default bakes the derived output. ---
c="$work_dir/c"; mkdir -p "$c"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$c/BUILD.bazel" --split-packages \
    >"$c/out" 2>"$c/err" || fail "control convert failed" "$c/err"
grep -qF 'write_file' "$c/BUILD.bazel" || fail "control should bake the derived output (write_file)" "$c/BUILD.bazel"
grep -qE '^genrule\(' "$c/BUILD.bazel" && fail "control must not emit a re-run genrule without the flag" "$c/BUILD.bazel"
echo "ok  meta-cmake-derived-codegen-rerun: default bakes the derived-name output"

# --- Flag: live genrule re-run. ---
r="$work_dir/r"; mkdir -p "$r"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$r/BUILD.bazel" --lift-derived-codegen --split-packages \
    >"$r/out" 2>"$r/err" || fail "--lift-derived-codegen convert failed" "$r/err"
b="$r/BUILD.bazel"
grep -qF 'write_file' "$b" && fail "the derived output should be a re-run genrule, not a bake, under the flag" "$b"
grep -qE '^genrule\(' "$b" || fail "expected a re-run genrule" "$b"
grep -qF 'outs = ["foo.gen.cc"]' "$b" || fail "genrule should declare the derived output" "$b"
grep -qF '$(RULEDIR)' "$b" || fail "re-run should cd into \$(RULEDIR) for the cwd-relative writer" "$b"
echo "ok  meta-cmake-derived-codegen-rerun: --lift-derived-codegen re-runs the tool as a live genrule"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-derived-codegen-rerun: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-derived-codegen-rerun: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/gen.py" "$fixture/foo.in" "$ws/"
cp "$b" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "derivedcodegen", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:use ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //:use failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-derived-codegen-rerun: //:use builds from the re-run genrule's output"
