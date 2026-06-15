#!/bin/sh
# meta-cmake-nested-cmake-in-script.sh — render+build gate for recovering a
# nested `cmake -S … -B …` configure+build hidden behind a BUILD-TIME `cmake -P`
# wrapper (the last residual of the wrapper-codegen arc).
#
# The fixture's add_custom_command runs `cmake -P wrap.cmake`; that script runs a
# nested `cmake -S sub -B subbuild` + `cmake --build`, invisible to the MAIN
# configure trace (it only runs when the converter re-traces the script). With
# --recognize-codegen --cmake-script-trace the converter re-traces the wrapper,
# records the nested (src,build) pair into NestedConfigureSink, and the warm
# second pass + lowerNestedBuilds lift the sub-build: the nested cc_library
# merges into the outer BUILD, the nested configure-generated header bakes, and
# the outer app links it — with NO `cmake -P` genrule and no cmake at Bazel build
# time. The re-trace runs the nested cmake, so this gate needs cmake + ninja at
# convert time (the standard render-gate guards).
#
# Control (no opt-in): the script stays an unrunnable standalone `cmake -P`
# genrule and the nested build is NOT lifted. Opt-in: the nested build is lifted
# (cc_library merged, no cmake -P genrule), then bazel-builds + RUNS //:app
# (exit 0 proves SUB_VALUE=42 flowed through the baked nested header).
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/nested-cmake-in-script"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: without the opt-in, the nested build is NOT lifted. ---
ctrl="$work_dir/ctrl"; mkdir -p "$ctrl"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$ctrl/BUILD.bazel" >"$ctrl/out" 2>"$ctrl/err" || true
if grep -qF 'cmake-codegen-nested-cmake' "$ctrl/BUILD.bazel" 2>/dev/null; then
    fail "control (no flags) should NOT lift the nested build" "$ctrl/BUILD.bazel"
fi
echo "ok  meta-cmake-nested-cmake-in-script: control does not lift the nested build (no opt-in)"

# --- Recover: re-trace the wrapper, lift the nested build. ---
rec="$work_dir/rec"; mkdir -p "$rec"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$rec/BUILD.bazel" --recognize-codegen --cmake-script-trace \
    >"$rec/out" 2>"$rec/err" || fail "recover convert (--recognize-codegen --cmake-script-trace) failed" "$rec/err"
b="$rec/BUILD.bazel"
grep -qF 'cmake-codegen-nested-cmake' "$b" || fail "expected the nested build to be lifted (cmake-codegen-nested-cmake tag)" "$b"
grep -qE '^cc_library\(' "$b" || fail "expected the merged nested cc_library" "$b"
grep -qF '":sublib"' "$b" || fail "the outer app should link the merged nested :sublib" "$b"
grep -E '^[[:space:]]*cmd = ' "$b" | grep -qF 'cmake' && fail "no cmake -P genrule should remain (the nested build lift owns it)" "$b"
echo "ok  meta-cmake-nested-cmake-in-script: nested cmake -S -B inside the cmake -P wrapper lifted -> merged cc_library + baked header"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-nested-cmake-in-script: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-nested-cmake-in-script: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws/sub"
cp "$fixture/main.c" "$ws/"
cp "$fixture/sub/sub.c" "$ws/sub/"
cp "$b" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "nestedcmakeinscript", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.7.1")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        run ${META_BAZEL_BUILD_ARGS:-} //:app ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building/running //:app failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-nested-cmake-in-script: //:app links the lifted nested sublib and runs clean (SUB_VALUE=42 via the baked nested header)"
