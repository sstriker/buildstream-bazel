#!/bin/sh
# Retry pass for fidelity rows dropped by the transient GitHub release-CDN 504
# outage. run-survey now uses a SHARED repository cache, so each attempt warms
# the cache for the next — residual CDN flakiness resolves across retries.
# Members with legitimately no CppCompile fidelity are NOT retried:
#   header-only (glm/nlohmann-json/eigen/cutlass), cuda-samples (.cu->CudaCompile),
#   zstd (real split-emit subpackage-label converter bug, not a 504).
set -u
cd /home/user/buildstream-bazel
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
export SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1
res=/home/user/survey-results; mkdir -p "$res"; log=/tmp/marathon4.log
say(){ echo "[$(date +%H:%M:%S)] $*" | tee -a "$log"; }

# member -> source dir
src_for(){ case "$1" in
  googletest) echo /tmp/googletest;; brotli) echo /tmp/brotli;;
  vtk) echo /tmp/vtk;; llvm) echo /tmp/llvm-project/llvm;;
  zlib) echo /tmp/zlib;; libxml2) echo /tmp/libxml2;; fmt) echo /tmp/fmt;;
  boost-core) echo /tmp/boost-core;; spdlog) echo /tmp/spdlog;;
  catch2) echo /tmp/Catch2;; openblas|OpenBLAS) echo /tmp/OpenBLAS;;
  *) echo "";; esac; }

try1(){ name="$1"; src="$2"; tmo="$3"
  rm -rf /tmp/survey-lens/"$name"
  SURVEY_BAZEL_BUILD="$name" SURVEY_BAZEL_BUILD_TIMEOUT="$tmo" timeout "$tmo" \
    sh scripts/run-survey.sh --out-dir /tmp/survey-lens "$name=$src" >>"$log" 2>&1
  if [ -s /tmp/survey-lens/"$name"/fidelity.json ]; then
    cp /tmp/survey-lens/"$name"/fidelity.json "$res/$name-fidelity.json"
    [ -s /tmp/survey-lens/"$name"/intent-capture.json ] && cp /tmp/survey-lens/"$name"/intent-capture.json "$res/$name-intent.json"
    rm -rf /tmp/survey-lens/"$name"/build-ws /tmp/survey-lens/"$name"/.bzcache /tmp/survey-lens/"$name"/cc-cmake 2>/dev/null
    return 0
  fi
  rm -rf /tmp/survey-lens/"$name"/build-ws /tmp/survey-lens/"$name"/.bzcache /tmp/survey-lens/"$name"/cc-cmake 2>/dev/null
  return 1
}

retry(){ name="$1"; tmo="$2"
  src="$(src_for "$name")"
  [ -n "$src" ] && [ -d "$src" ] || { say "SKIP $name (no src)"; return; }
  [ -s "$res/$name-fidelity.json" ] && { say "OK $name (fidelity already present)"; return; }
  i=1
  while [ "$i" -le 3 ]; do
    say "START $name attempt $i (tmo=${tmo}s)"
    if try1 "$name" "$src" "$tmo"; then say "DONE $name attempt $i -> fidelity OK"; return; fi
    say "RETRY $name attempt $i -> still no fidelity"
    i=$((i+1))
  done
  say "GAVE UP $name after 3 attempts (fidelity still absent)"
}

say "MARATHON4 START (504 fidelity retries; shared repo cache)"
# small/medium first to warm the shared cache cheaply, then the heavies
retry googletest 1200
retry brotli 1200
retry vtk 7200
retry llvm 7200
say "MARATHON4 DONE"
