#!/bin/sh
# Emit the generated sources into the OUTPUT_DIR passed as $1.
set -eu
out="$1"
printf '#include "foo.h"\nint gen_value(void) { return 7; }\n' > "$out/foo.c"
printf 'int gen_value(void);\n' > "$out/foo.h"
printf '# manifest\n' > "$out/manifest.cmake"
