#!/bin/sh
# meta-cmake-standalone-flag-output-dir.sh — render+build gate for the
# output-DIRECTORY-flag anchoring on the STANDALONE custom-command path.
#
# An unrecognized derived-output codegen tool (gen.sh writes
# <out-dir>/greeting.cpp, name DERIVED from --out-dir, not in argv) is run
# DIRECTLY by a custom command and consumed via add_custom_target — so it lowers
# through the standalone path, not the per-target genrule path. The output-dir
# flag must anchor to $(RULEDIR) (anchorGenruleOutputs); before that anchoring was
# shared onto the standalone path, the genrule wrote to the exec-root cwd and
# Bazel failed on the missing declared output. No opt-in flags — standalone
# genrule emission is default-on.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/standalone-flag-output-dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

rec="$work_dir/rec"; mkdir -p "$rec"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$rec/BUILD.bazel" >"$rec/out" 2>"$rec/err" || fail "convert failed" "$rec/err"
b="$rec/BUILD.bazel"
grep -qE '^genrule\(' "$b" || fail "expected a standalone genrule" "$b"
grep -qF '"greeting.cpp"' "$b" || fail "genrule should declare greeting.cpp as an out" "$b"
grep -qF -- '--out-dir=$(RULEDIR)' "$b" || fail "the output dir must anchor to \$(RULEDIR) on the standalone path" "$b"
echo "ok  meta-cmake-standalone-flag-output-dir: derived-output dir flag anchored to \$(RULEDIR) on the standalone path"

# --- Bazel-build half: the genrule must produce greeting.cpp. ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-standalone-flag-output-dir: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-standalone-flag-output-dir: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/gen.sh" "$fixture/greeting.def" "$ws/"
cp "$b" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "standaloneflagoutdir", version = "0.0.0")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:custom_command_greeting_cpp ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the standalone genrule failed (output-dir not anchored?)"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-standalone-flag-output-dir: the standalone genrule produces greeting.cpp under \$(RULEDIR)"
