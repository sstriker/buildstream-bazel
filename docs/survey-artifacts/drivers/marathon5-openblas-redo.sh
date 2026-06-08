#!/bin/sh
# OpenBLAS redo: marathon3 ran it as "OpenBLAS", but the conf is openblas.conf
# (lowercase). Without the conf, openblas.conf's DIAG_CONVERT_FLAGS (which
# dead-branches the getarch execute_process probes) wasn't applied, so the 1
# getarch rejection remained and the skip(rej) gate returned before the
# fidelity + intent lenses ran. Re-run with the correct lowercase token.
set -u
cd /home/user/buildstream-bazel
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
export SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1
res=/home/user/survey-results; mkdir -p "$res"; log=/tmp/marathon5.log
say(){ echo "[$(date +%H:%M:%S)] $*" | tee -a "$log"; }
name=openblas; src=/tmp/OpenBLAS; tmo=3600
say "MARATHON5 START (openblas redo, correct conf token)"
rm -rf /tmp/survey-lens/"$name"
say "START $name (tmo=${tmo}s)"
SURVEY_BAZEL_BUILD="$name" SURVEY_BAZEL_BUILD_TIMEOUT="$tmo" timeout "$tmo" \
  sh scripts/run-survey.sh --out-dir /tmp/survey-lens "$name=$src" >>"$log" 2>&1
rc=$?
[ -s /tmp/survey-lens/"$name"/fidelity.json ] && cp /tmp/survey-lens/"$name"/fidelity.json "$res/$name-fidelity.json"
[ -s /tmp/survey-lens/"$name"/intent-capture.json ] && cp /tmp/survey-lens/"$name"/intent-capture.json "$res/$name-intent.json"
row=$(grep -E "^$name " /tmp/survey-lens/summary.txt 2>/dev/null | tail -1)
fj=$([ -s "$res/$name-fidelity.json" ] && echo F); ij=$([ -s "$res/$name-intent.json" ] && echo I)
say "DONE $name rc=$rc out=${fj:-.}${ij:-.} :: ${row:-<no row>}"
rm -rf /tmp/survey-lens/"$name"/build-ws /tmp/survey-lens/"$name"/.bzcache /tmp/survey-lens/"$name"/cc-cmake 2>/dev/null
say "MARATHON5 DONE"
