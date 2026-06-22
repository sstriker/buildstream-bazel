#!/usr/bin/env python3
# Writes value.c to the CURRENT WORKING DIRECTORY (the temp dir the wrapper sets).
with open("value.c", "w") as f:
    f.write("int gen_value(void) { return 7; }\n")
