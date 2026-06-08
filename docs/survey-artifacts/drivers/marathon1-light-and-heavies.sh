#!/bin/sh
set -u
cd /home/user/buildstream-bazel
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
export SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1
export BSB_CUDA_ROOT=/opt/cuda-root BSB_CUDA_HOST_CC=/usr/bin/gcc-12
res=/home/user/survey-results; mkdir -p "$res"
log=/tmp/marathon.log
say(){ echo "[$(date +%H:%M:%S)] $*" | tee -a "$log"; }
clone(){ [ -d "$2" ] || git clone --depth 1 ${3:+--branch "$3"} "$1" "$2" >>"$log" 2>&1; }

run(){ # name src timeout
  name="$1"; src="$2"; tmo="$3"
  if [ ! -d "$src" ]; then say "SKIP $name (no src $src)"; return; fi
  rm -rf /tmp/survey-lens/"$name"
  say "START $name (src=$src tmo=${tmo}s)"
  SURVEY_BAZEL_BUILD="$name" timeout "$tmo" sh scripts/run-survey.sh --out-dir /tmp/survey-lens "$name=$src" >>"$log" 2>&1
  rc=$?
  cp /tmp/survey-lens/"$name"/fidelity.json "$res/$name-fidelity.json" 2>/dev/null
  cp /tmp/survey-lens/"$name"/intent-capture.json "$res/$name-intent.json" 2>/dev/null
  row=$(grep -E "^$name " /tmp/survey-lens/summary.txt 2>/dev/null | tail -1)
  fj=$([ -f "$res/$name-fidelity.json" ] && echo F); ij=$([ -f "$res/$name-intent.json" ] && echo I)
  say "DONE $name rc=$rc out=${fj:-.}${ij:-.} :: ${row:-<no row>}"
  rm -rf /tmp/survey-lens/"$name"/build-ws /tmp/survey-lens/"$name"/.bzcache /tmp/survey-lens/"$name"/cc-cmake /tmp/survey-lens/"$name"/convert.log 2>/dev/null
}

say "MARATHON START"
# light/moderate (repo-root sources, already cloned)
run glog /tmp/glog 1200
run libpng /tmp/libpng 1200
run libevent /tmp/libevent 1200
( cd /tmp/mbedtls && git submodule update --init --recursive >>"$log" 2>&1 ) 2>/dev/null
run mbedtls /tmp/mbedtls 1500
run abseil /tmp/abseil-cpp 1800
run curl /tmp/curl 1500
run sdl /tmp/SDL 1800
run zstd /tmp/zstd/build/cmake 1200
run protobuf /tmp/protobuf 1800
run cuda-samples /tmp/cuda-samples/cpp/0_Introduction/vectorAdd 1200
# heavies (clone on demand; generous timeouts; reclaim clone after)
clone https://github.com/NVIDIA/cutlass.git /tmp/cutlass
run cutlass /tmp/cutlass 2400
clone https://github.com/Kitware/VTK.git /tmp/vtk
run vtk /tmp/vtk 3000
clone https://github.com/llvm/llvm-project.git /tmp/llvm-project
run llvm /tmp/llvm-project/llvm 3000
say "MARATHON DONE"
