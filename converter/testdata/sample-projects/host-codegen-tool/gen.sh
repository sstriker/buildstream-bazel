#!/bin/sh
# Minimal host codegen tool: wrap the input's first line into a C string
# constant. Stands in for a project's own python/perl/flatc-style generator
# that has no native Bazel rule.
set -eu
in="$1"; out="$2"
printf 'const char *greeting = "%s";\n' "$(cat "$in")" > "$out"
