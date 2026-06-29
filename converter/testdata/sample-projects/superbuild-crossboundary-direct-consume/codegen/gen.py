#!/usr/bin/env python3
# In-tree codegen tool: writes the generated source to the path named in argv
# (the OUTER build tree, passed by the nested custom command).
import sys

with open(sys.argv[1], "w") as f:
    f.write("int gen_value(void) { return 7; }\n")
