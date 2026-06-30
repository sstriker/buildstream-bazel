#!/usr/bin/env python3
# Writes foo.c / foo.h into the CURRENT WORKING DIRECTORY (the temp dir the
# wrapper sets via WORKING_DIRECTORY), naming no OUTPUT_DIR in argv.
with open("foo.c", "w") as f:
    f.write('#include "foo.h"\nint foo_value(void) { return 7; }\n')
with open("foo.h", "w") as f:
    f.write("int foo_value(void);\n")
