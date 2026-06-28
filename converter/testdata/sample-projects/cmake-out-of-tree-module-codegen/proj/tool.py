#!/usr/bin/env python3
# In-tree codegen tool driven by the out-of-tree module. Writes gen_value() to
# the path named in argv (the project's build dir).
import sys

with open(sys.argv[1], "w") as f:
    f.write("int gen_value(void) { return 7; }\n")
