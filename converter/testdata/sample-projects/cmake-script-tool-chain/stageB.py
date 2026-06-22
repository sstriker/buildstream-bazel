#!/usr/bin/env python3
# stage B: read the intermediate, write the generated C source.
import sys
with open(sys.argv[1]) as f:
    n = int(f.read().strip())
with open(sys.argv[2], "w") as f:
    f.write("int gen_value(void) { return %d; }\n" % n)
