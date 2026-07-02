#!/bin/sh
# meta-cmake-recognizer-starlark-stdout.sh — gate for the Starlark genrule(...)
# builtin, via a STDOUT-GENERATOR operator recognizer.
#
# The fixture's tool `sgen` writes generated C to stdout, captured by
# execute_process(... OUTPUT_FILE gen.h). It has NO native Bazel rule, so the
# built-in protoc/grpc recognizers can't claim it. The fixture ships a
# recognizer.star that matches `sgen` and lowers it — via the new genrule(...)
# builtin — to a genrule re-running the tool at Bazel time (`$(location
# //tools:sgen) $(SRCS) > $@`).
#
# Control (--recognize-codegen, recognizers NOT loaded): the OUTPUT_FILE call
# falls to the raw-host hoist — a genrule driving the bare host `sgen` (non-
# hermetic, + a host-codegen-tool todo).
# Operator (--recognizers <star>): the genrule drives the hermetic //tools:sgen
# tool label, and the consumer keeps gen.h in its srcs (genrule output resolved by
# filename — a genrule has no CcInfo to hang a deps edge on).
#
# Then bazel-builds //:app (main.c #includes gen.h) to prove the emitted genrule
# is real. `sgen` must be on PATH so the converter's configure produces gen.h.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/recognizer-starlark-stdout"
star="$fixture/recognizer.star"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# sgen on PATH so the configure's execute_process produces gen.h on disk (the
# on-disk corroboration the OUTPUT_FILE recognition needs).
export PATH="$fixture:$PATH"

fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

# --- Control: no recognizers → raw-host hoist of the bare `sgen`. ---
ctrl_dir="$work_dir/ctrl"; mkdir -p "$ctrl_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" --out-build "$ctrl_dir/BUILD.bazel" \
    --recognize-codegen \
    >"$work_dir/ctrl.out" 2>"$work_dir/ctrl.err" \
    || fail "control convert exited non-zero" "$work_dir/ctrl.err"
grep -qF 'name = "exec_gen_h"' "$ctrl_dir/BUILD.bazel" \
    || fail "control (no recognizer) should emit the raw-host hoist genrule exec_gen_h" "$ctrl_dir/BUILD.bazel"
grep -qF 'sgen $(location in.x)' "$ctrl_dir/BUILD.bazel" \
    || fail "control (no recognizer) should hoist the bare host sgen" "$ctrl_dir/BUILD.bazel"
grep -qF '//tools:sgen' "$ctrl_dir/BUILD.bazel" \
    && fail "control must NOT reference the hermetic //tools:sgen without the operator recognizer" "$ctrl_dir/BUILD.bazel"
echo "ok  meta-cmake-recognizer-starlark-stdout: sgen falls to the raw-host hoist without the operator recognizer"

# --- Operator: the genrule(...) builtin lowers sgen to a hermetic tool genrule. ---
rec_dir="$work_dir/rec"; mkdir -p "$rec_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" --out-build "$rec_dir/BUILD.bazel" \
    --recognize-codegen --recognizers "$star" \
    >"$work_dir/rec.out" 2>"$work_dir/rec.err" \
    || fail "operator-recognizer convert exited non-zero" "$work_dir/rec.err"
build="$rec_dir/BUILD.bazel"
grep -qE '^\s*name = "sgen_gen_h",' "$build" || fail "sgen not lowered to the recognizer's genrule sgen_gen_h" "$build"
grep -qF '$(location //tools:sgen) $(SRCS) > $@' "$build" || fail "the genrule should re-run the hermetic //tools:sgen tool" "$build"
grep -qF '"gen.h"' "$build" || fail "gen.h not declared by the genrule" "$build"
# The consumer keeps gen.h in srcs (genrule output by filename), NOT a deps edge.
# Scope the check to the cc_binary(name = "app") stanza specifically (paragraph
# mode, stanzas are blank-line separated) so a bare '"gen.h"' elsewhere — e.g. the
# genrule's own outs — can't satisfy it: a real regression stripping gen.h from
# the consumer must fail here.
app_stanza=$(awk 'BEGIN{RS="";ORS="\n\n"} /name = "app"/' "$build")
printf '%s' "$app_stanza" | grep -qF '"gen.h"' \
    || fail "consumer app must keep gen.h in srcs (genrule has no CcInfo for a deps edge)" "$build"
# Whitespace-tolerant: no cc deps edge onto the genrule anywhere.
grep -qE 'deps[[:space:]]*=[[:space:]]*\[[[:space:]]*":sgen_gen_h"' "$build" \
    && fail "a genrule output must NOT be wired via a cc deps edge" "$build"
echo "ok  meta-cmake-recognizer-starlark-stdout: sgen lowered to a hermetic genrule via the genrule(...) builtin (consumer keeps gen.h in srcs)"

# --- Bazel-build half: the genrule + //tools:sgen are real. ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-recognizer-starlark-stdout: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-recognizer-starlark-stdout: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws/tools"
cp "$fixture/main.c" "$fixture/in.x" "$ws/"
cp "$build" "$ws/BUILD.bazel"
# The Bazel-time tool is a cc_binary twin of the convert-time `sgen` shell script
# (rules_cc is already a dep; avoids an extra rules_shell dep for sh_binary under
# bazel >= 9). Both emit `#define GEN_VALUE <n>` from the spec.
cp "$fixture/sgen.c" "$ws/tools/sgen.c"
cat > "$ws/tools/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(name = "sgen", srcs = ["sgen.c"], visibility = ["//visibility:public"])
EOF
cat > "$ws/MODULE.bazel" <<'EOF'
module(name = "recognizerstarlarkstdout", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        run ${META_BAZEL_BUILD_ARGS:-} //:app ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: running //:app failed (the genrule didn't regenerate gen.h from //tools:sgen?)"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-recognizer-starlark-stdout: //:app builds + runs from the genrule(...)-emitted rule (GEN_VALUE==7)"
