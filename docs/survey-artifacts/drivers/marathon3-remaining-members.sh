#!/bin/sh
# Third pass: the Status-at-a-glance members not covered by marathon1/2, so
# every corpus row gets a fidelity + intent snapshot. Clones already in /tmp.
set -u
cd /home/user/buildstream-bazel
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
export SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1
res=/home/user/survey-results; mkdir -p "$res"; log=/tmp/marathon3.log
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
say "MARATHON3 START"
run fmt /tmp/fmt 1200
run libxml2 /tmp/libxml2 1500
run brotli /tmp/brotli 1200
run googletest /tmp/googletest 1200
run zlib /tmp/zlib 1200
run boost-core /tmp/boost-core 1200
run spdlog /tmp/spdlog 1200
run catch2 /tmp/Catch2 1500
run OpenBLAS /tmp/OpenBLAS 3600
say "MARATHON3 DONE"
