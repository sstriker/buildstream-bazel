#!/usr/bin/env python3
# A codegen tool that writes value.c to its CURRENT WORKING DIRECTORY, naming no
# final path in argv — the write-in-place pattern. The cmake -P wrapper runs it
# with WORKING_DIRECTORY == the declared output dir, so value.c lands in place
# with no relocation.
with open("value.c", "w") as f:
    f.write("int gen_value(void) { return 9; }\n")
