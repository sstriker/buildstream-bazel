#!/usr/bin/env python3
# A codegen tool that writes value.c to its CURRENT WORKING DIRECTORY, naming no
# final path in argv — the temp-dir pattern. The cmake -P wrapper runs it with
# WORKING_DIRECTORY=<tmp> and then relocates the file.
with open("value.c", "w") as f:
    f.write("int gen_value(void) { return 7; }\n")
