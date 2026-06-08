#!/bin/sh
set -u
cd /home/user/buildstream-bazel
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
export SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1
export BSB_CUDA_ROOT=/opt/cuda-root BSB_CUDA_HOST_CC=/usr/bin/gcc-12
res=/home/user/survey-results; mkdir -p "$res"; log=/tmp/marathon2.log
say(){ echo "[$(date +%H:%M:%S)] $*" | tee -a "$log"; }
run(){ name="$1"; src="$2"; tmo="$3"
  [ -d "$src" ] || { say "SKIP $name (no src $src)"; return; }
  rm -rf /tmp/survey-lens/"$name"
  say "START $name (tmo=${tmo}s)"
  SURVEY_BAZEL_BUILD="$name" timeout "$tmo" sh scripts/run-survey.sh --out-dir /tmp/survey-lens "$name=$src" >>"$log" 2>&1
  rc=$?
  cp /tmp/survey-lens/"$name"/fidelity.json "$res/$name-fidelity.json" 2>/dev/null
  cp /tmp/survey-lens/"$name"/intent-capture.json "$res/$name-intent.json" 2>/dev/null
  row=$(grep -E "^$name " /tmp/survey-lens/summary.txt 2>/dev/null | tail -1)
  fj=$([ -f "$res/$name-fidelity.json" ] && echo F); ij=$([ -f "$res/$name-intent.json" ] && echo I)
  say "DONE $name rc=$rc out=${fj:-.}${ij:-.} :: ${row:-<no row>}"
  rm -rf /tmp/survey-lens/"$name"/build-ws /tmp/survey-lens/"$name"/.bzcache /tmp/survey-lens/"$name"/cc-cmake /tmp/survey-lens/"$name"/convert.log 2>/dev/null
}
say "MARATHON2 START (redos + heavies, generous budgets)"
run libevent /tmp/libevent 1800
run zstd /tmp/zstd/build/cmake 1800
run vtk /tmp/vtk 7200
[ -d /tmp/llvm-project ] || git clone --depth 1 https://github.com/llvm/llvm-project.git /tmp/llvm-project >>"$log" 2>&1
run llvm /tmp/llvm-project/llvm 7200
say "MARATHON2 DONE"
