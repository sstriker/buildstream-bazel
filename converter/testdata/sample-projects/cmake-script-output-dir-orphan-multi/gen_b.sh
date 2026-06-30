#!/bin/sh
# Emit edge B's generated sources into OUTPUT_DIR ($1 == gen/b).
set -eu
out="$1"
printf '#include "bar.h"\nint bar_value(void) { return 11; }\n' > "$out/bar.c"
printf 'int bar_value(void);\n' > "$out/bar.h"
printf '# manifest b\n' > "$out/manifest.cmake"
