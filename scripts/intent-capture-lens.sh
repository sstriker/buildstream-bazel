#!/bin/sh
# intent-capture-lens.sh — the agent-as-oracle "what did we miss?" survey lens
# (see the intent-capture lens item in ROADMAP.md).
#
# Pipeline (the LLM judgment in the middle is PLUGGABLE so CI can stub it and
# operators can swap judges):
#
#   intent-lens prompt  ──▶  $INTENT_LENS_JUDGE  ──▶  intent-lens triage
#     (deterministic)        (your model call)        (deterministic dedup)
#
# Usage:
#   INTENT_LENS_JUDGE='claude -p' \
#     scripts/intent-capture-lens.sh <converted-dir> <cmake-src> <out-dir> [element]
#
# <converted-dir> holds the rendered BUILD.bazel + MODULE.bazel (and, by
# default, conversion-todos.json — the converter writes it next to BUILD.bazel).
# rejections.json is read from <converted-dir> or <out-dir> if present.
# $INTENT_LENS_JUDGE is a command that reads the prompt on stdin and writes the
# findings JSON on stdout. Output lands at <out-dir>/intent-capture.json.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"

if [ "$#" -lt 3 ]; then
  echo "usage: $0 <converted-dir> <cmake-src> <out-dir> [element]" >&2
  exit 2
fi
converted="$1"; cmake_src="$2"; out_dir="$3"; element="${4:-$(basename "$cmake_src")}"

if [ -z "${INTENT_LENS_JUDGE:-}" ]; then
  echo "skip: INTENT_LENS_JUDGE not set (e.g. INTENT_LENS_JUDGE='claude -p'); nothing to run" >&2
  exit 0
fi

command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }

mkdir -p "$out_dir"
bin="$repo_root/build/bin/intent-lens"
mkdir -p "$(dirname "$bin")"
( cd "$repo_root" && go build -o "$bin" ./converter/cmd/intent-lens )

# Locate the already-flagged reports (best-effort; empty is fine).
todos=""
for c in "$converted/conversion-todos.json" "$out_dir/conversion-todos.json"; do
  [ -f "$c" ] && { todos="$c"; break; }
done
rejections=""
for c in "$converted/rejections.json" "$out_dir/rejections.json"; do
  [ -f "$c" ] && { rejections="$c"; break; }
done

prompt="$out_dir/intent-prompt.txt"
findings="$out_dir/intent-findings.json"
report="$out_dir/intent-capture.json"

"$bin" prompt --converted "$converted" --cmake-src "$cmake_src" \
  ${todos:+--todos "$todos"} ${rejections:+--rejections "$rejections"} \
  --element "$element" --out "$prompt"

# The pluggable judge: prompt on stdin, findings JSON on stdout. Word-split
# INTENT_LENS_JUDGE so callers can pass flags (e.g. 'claude -p').
# shellcheck disable=SC2086
$INTENT_LENS_JUDGE < "$prompt" > "$findings"

"$bin" triage --findings "$findings" \
  ${todos:+--todos "$todos"} ${rejections:+--rejections "$rejections"} \
  --element "$element" --out "$report"

echo "  $element: intent-capture -> $report" >&2
