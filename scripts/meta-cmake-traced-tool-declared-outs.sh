#!/bin/sh
# meta-cmake-traced-tool-declared-outs.sh — render+build gate for the
# declared-output direct-tool genrule lift: an UNRECOGNIZED codegen tool hidden
# inside a `cmake -P` wrapper, recovered as a runner-free genrule using the
# wrapping custom command's DECLARED output as the authority.
#
# The fixture's add_custom_command runs `cmake -P gen_wrap.cmake`; that wrapper's
# execute_process runs `sh gen.sh --out-dir=<bin> greeting.def`. gen.sh DERIVES
# its output name (greeting.cpp) from the --out-dir flag, so no recognizer claims
# it and the argv-output lift can't read it — but the OUTPUT clause names
# greeting.cpp, which is enough data to lift. With --recognize-codegen
# --cmake-script-trace the converter re-traces the wrapper, sees the gen.sh call,
# and emits a genrule running gen.sh directly with --out-dir → $(RULEDIR) and
# greeting.def staged as a src. No --cmake-script-runner: it runs the real tool,
# not `cmake -P`. The trace runs gen.sh (writing greeting.cpp to the build dir,
# the on-disk evidence corroborating the declaration), so this gate needs a POSIX
# sh at convert time (always present).
#
# Control (no opt-in): the cmake -P custom command is an unrecoverable Tier-1
# refusal. Gating: --recognize-codegen WITHOUT --cmake-script-trace still refuses
# (the recovery needs the trace). Recover: the genrule renders with the rewritten
# cmd; then bazel-builds //:use_greeting.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/traced-tool-declared-outs"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: without the opt-in, the cmake -P custom command refuses. ---
ctrl="$work_dir/ctrl"; mkdir -p "$ctrl"
if "$bin_dir/convert-element-cmake" --source-root "$fixture" \
        --out-build "$ctrl/BUILD.bazel" >"$ctrl/out" 2>"$ctrl/err"; then
    fail "control (no flags) should refuse the cmake -P custom command, but convert succeeded" "$ctrl/err"
fi
grep -qF 'cmake -P' "$ctrl/err" || fail "control refusal should name the cmake -P script" "$ctrl/err"
echo "ok  meta-cmake-traced-tool-declared-outs: control refuses the cmake -P-wrapped tool (unrecoverable without opt-in)"

# --- Gating: --recognize-codegen alone (no trace) still refuses. ---
gate="$work_dir/gate"; mkdir -p "$gate"
if "$bin_dir/convert-element-cmake" --source-root "$fixture" \
        --out-build "$gate/BUILD.bazel" --recognize-codegen >"$gate/out" 2>"$gate/err"; then
    fail "the recovery must be gated on --cmake-script-trace, but --recognize-codegen alone recovered it" "$gate/err"
fi
echo "ok  meta-cmake-traced-tool-declared-outs: the recovery is gated on --cmake-script-trace (recognize-codegen alone still refuses)"

# --- Recover: declared-output direct-tool genrule, no runner. ---
rec="$work_dir/rec"; mkdir -p "$rec"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$rec/BUILD.bazel" --recognize-codegen --cmake-script-trace \
    >"$rec/out" 2>"$rec/err" || fail "recover convert (--recognize-codegen --cmake-script-trace) failed" "$rec/err"
b="$rec/BUILD.bazel"
grep -qE '^genrule\(' "$b" || fail "expected a direct-tool genrule" "$b"
grep -qF '"greeting.cpp"' "$b" || fail "genrule should declare greeting.cpp as an out" "$b"
grep -qF -- '--out-dir=$(RULEDIR)' "$b" || fail "the output dir should be rewritten to \$(RULEDIR) (shared anchoring)" "$b"
grep -qF '"greeting.def"' "$b" || fail "greeting.def should be a genrule src" "$b"
grep -qF '"gen.sh"' "$b" || fail "gen.sh should be a genrule src" "$b"
# The genrule runs the tool directly — its cmd must NOT shell out to cmake -P.
# (Match the cmd line specifically; the carried CMakeLists comment legitimately
# mentions "cmake -P".)
grep -E '^[[:space:]]*cmd = ' "$b" | grep -qF 'cmake' && fail "the genrule cmd should run the tool directly, not cmake" "$b"
echo "ok  meta-cmake-traced-tool-declared-outs: unrecognized tool recovered to a runner-free direct-tool genrule (reusing the shared emission)"

# --- Widening: a build-SUBDIR output dir anchors to $(RULEDIR)/<subdir>. ---
grep -qF '"gen/greeting.cpp"' "$b" || fail "expected the subdir codegen out gen/greeting.cpp" "$b"
grep -qF -- '--out-dir=$(RULEDIR)/gen' "$b" || fail "the subdir output dir should anchor to \$(RULEDIR)/gen" "$b"
echo "ok  meta-cmake-traced-tool-declared-outs: build-subdir output dir recovered (--out-dir=\$(RULEDIR)/gen)"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-traced-tool-declared-outs: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-traced-tool-declared-outs: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws"
cp "$fixture/gen.sh" "$fixture/greeting.def" "$fixture/gen_wrap.cmake" "$fixture/gen_wrap_sub.cmake" \
   "$fixture/use_greeting.cc" "$fixture/use_greeting_sub.cc" "$ws/"
cp "$b" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "tracedtooldeclaredouts", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:use_greeting //:use_greeting_sub ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the recovered libs failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-traced-tool-declared-outs: //:use_greeting + //:use_greeting_sub build from the runner-free direct-tool genrules (build-root + subdir)"
