#!/bin/sh
# compile-commands-lens.sh <name> <src> — the compile-commands FIDELITY lens.
#
# Compares, per translation unit, what cmake would compile a source with
# against what the converted Bazel graph actually compiles it with — catching
# macro-define / language-standard drift that a green build hides (a wrongly
# propagated PRIVATE define, a missing -D that flips an #ifdef, the shared-lib
# DEFINE_SYMBOL export macro leaking to consumers, …).
#
# Three steps:
#   1. cmake ground truth: configure <src> with CMAKE_EXPORT_COMPILE_COMMANDS=ON
#      (static, mirroring the build lens's BUILD_SHARED_LIBS=OFF default + any
#      <name>.conf cmake-defines) -> <out>/cmake/compile_commands.json.
#   2. Bazel view: run the build lens for <name> (converts + builds the project-B
#      graph), then `bazel aquery --output=jsonproto mnemonic(CppCompile,//...)`
#      over the built build-ws -> <out>/aquery.json.
#   3. Diff with cmd/compile-commands-diff -> per-TU define/-std report.
#
# Usage:  sh scripts/compile-commands-lens.sh <name> <src> [out-dir]
# Env:    honors the same SURVEY_* knobs run-survey.sh does.
set -eu

name="${1:?usage: compile-commands-lens.sh <name> <src> [out-dir]}"
src="${2:?usage: compile-commands-lens.sh <name> <src> [out-dir]}"
out="${3:-/tmp/cc-lens-$name}"
repo_root=$(cd "$(dirname "$0")/.." && pwd)

log() { printf '[cc-lens %s] %s\n' "$name" "$*" >&2; }
mkdir -p "$out"

# Per-project cmake-defines from the build-lens .conf (so the cmake ground truth
# matches what the converter saw). Sourced for CONVERT_FLAGS; we extract the
# --cmake-define pairs. BUILD_SHARED_LIBS=OFF mirrors the build lens default.
conf="$repo_root/scripts/build-lens/$name.conf"
cmake_defs="-DBUILD_SHARED_LIBS=OFF"
if [ -f "$conf" ]; then
    CONVERT_FLAGS=""
    # shellcheck disable=SC1090
    . "$conf"
    # Pull `--cmake-define K=V` pairs out of CONVERT_FLAGS into -DK=V.
    set -- $CONVERT_FLAGS
    while [ $# -gt 0 ]; do
        if [ "$1" = "--cmake-define" ] && [ $# -ge 2 ]; then
            cmake_defs="$cmake_defs -D$2"
            shift 2
        else
            shift
        fi
    done
fi

# --- 1. cmake ground truth -------------------------------------------------
log "configuring cmake (EXPORT_COMPILE_COMMANDS; $cmake_defs)"
rm -rf "$out/cmake"; mkdir -p "$out/cmake"
# shellcheck disable=SC2086
if ! cmake -S "$src" -B "$out/cmake" -G Ninja -DCMAKE_EXPORT_COMPILE_COMMANDS=ON $cmake_defs >"$out/cmake-configure.log" 2>&1; then
    log "cmake configure FAILED — see $out/cmake-configure.log"; exit 1
fi
[ -f "$out/cmake/compile_commands.json" ] || { log "no cmake compile_commands.json produced"; exit 1; }

# --- 2. Bazel view (build lens -> aquery) ----------------------------------
log "building converted graph via the build lens"
SURVEY_BAZEL_BUILD="$name" sh "$repo_root/scripts/run-survey.sh" --out-dir "$out/survey" "$name=$src" >"$out/build-lens.log" 2>&1 || true
ws="$out/survey/$name/build-ws"
[ -d "$ws" ] || { log "build-ws not produced (convert/skip?) — see $out/build-lens.log"; exit 1; }

bzl=bazel; command -v bazel >/dev/null 2>&1 || bzl=bazelisk
log "aquery CppCompile actions"
( cd "$ws" && "$bzl" --output_user_root="$out/survey/$name/.bzcache" --noworkspace_rc \
    aquery --output=jsonproto 'mnemonic("CppCompile", //...)' ) >"$out/aquery.json" 2>"$out/aquery.err" || {
    log "aquery FAILED — see $out/aquery.err"; exit 1; }

# --- 3. diff ----------------------------------------------------------------
diff_bin="$repo_root/build/bin/compile-commands-diff"
if ! ( cd "$repo_root" && go build -o "$diff_bin" ./converter/cmd/compile-commands-diff ) 2>/dev/null; then
    diff_bin="go run $repo_root/converter/cmd/compile-commands-diff"
fi
log "diffing"
# shellcheck disable=SC2086
$diff_bin --cmake "$out/cmake/compile_commands.json" --aquery "$out/aquery.json" --json "$out/fidelity.json"
log "report: $out/fidelity.json"
