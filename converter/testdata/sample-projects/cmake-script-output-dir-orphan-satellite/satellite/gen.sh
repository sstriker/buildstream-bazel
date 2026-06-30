#!/bin/sh
set -eu
out="$1"
printf '#include "foo.h"\nint gen_value(void) { return 7; }\n' > "$out/foo.c"
printf 'int gen_value(void);\n' > "$out/foo.h"
printf '# manifest\n' > "$out/manifest.cmake"
