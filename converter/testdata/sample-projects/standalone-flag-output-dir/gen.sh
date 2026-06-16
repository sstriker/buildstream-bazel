#!/bin/sh
# Unrecognized derived-output codegen tool: writes <out-dir>/greeting.cpp; the
# output name is DERIVED from --out-dir (not named in argv), so only the
# output-DIR-flag anchoring puts it under $(RULEDIR).
set -eu
out_dir=""; in_file=""
for a in "$@"; do case "$a" in --out-dir=*) out_dir="${a#--out-dir=}";; *) in_file="$a";; esac; done
[ -n "$out_dir" ] && [ -n "$in_file" ] || { echo "usage: gen.sh --out-dir=DIR INPUT" >&2; exit 2; }
mkdir -p "$out_dir"
printf 'const char *greeting(void) { return "%s"; }\n' "$(cat "$in_file")" > "$out_dir/greeting.cpp"
