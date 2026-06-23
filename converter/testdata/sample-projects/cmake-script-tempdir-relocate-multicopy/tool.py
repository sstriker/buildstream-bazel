#!/usr/bin/env python3
# A codegen tool that writes TWO sources (a.c, b.c) to its CURRENT WORKING
# DIRECTORY, naming no final path in argv — the temp-dir pattern. The cmake -P
# wrapper runs it with WORKING_DIRECTORY=<tmp> and then relocates both files
# into the declared output directory with one multi-source `cmake -E copy`.
with open("a.c", "w") as f:
    f.write("int gen_a(void) { return 3; }\n")
with open("b.c", "w") as f:
    f.write("int gen_b(void) { return 4; }\n")
