#!/usr/bin/env python3
# Writes value.h into its CURRENT WORKING DIRECTORY (the mktemp tempdir); the
# cmake -P wrapper then relocates it to the declared output.
with open("value.h", "w") as f:
    f.write("#define GEN_VALUE 7\n")
