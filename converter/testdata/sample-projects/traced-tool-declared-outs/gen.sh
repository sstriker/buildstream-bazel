#!/bin/sh
# Unrecognized codegen tool: reads a .def file and writes <out-dir>/greeting.cpp.
# The output filename is DERIVED (not named in argv), so only the wrapping custom
# command's OUTPUT clause tells the converter what lands here.
set -eu
out_dir=""
in_file=""
for a in "$@"; do
    case "$a" in
        --out-dir=*) out_dir="${a#--out-dir=}" ;;
        *) in_file="$a" ;;
    esac
done
[ -n "$out_dir" ] && [ -n "$in_file" ] || { echo "usage: gen.sh --out-dir=DIR INPUT" >&2; exit 2; }
msg="$(cat "$in_file")"
printf 'const char *greeting() { return "%s"; }\n' "$msg" > "$out_dir/greeting.cpp"
