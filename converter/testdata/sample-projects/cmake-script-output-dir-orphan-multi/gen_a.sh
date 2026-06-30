#!/bin/sh
# Emit edge A's generated sources into OUTPUT_DIR ($1 == gen/a).
set -eu
out="$1"
printf '#include "foo.h"\nint foo_value(void) { return 7; }\n' > "$out/foo.c"
printf 'int foo_value(void);\n' > "$out/foo.h"
printf '# manifest a\n' > "$out/manifest.cmake"
