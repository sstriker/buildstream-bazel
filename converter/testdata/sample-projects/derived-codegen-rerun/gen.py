#!/usr/bin/env python3
import sys, os
inp = sys.argv[1]
stem = os.path.basename(inp).rsplit(".", 1)[0]
with open(stem + ".gen.cc", "w") as f:
    f.write("int derived_ok() { return 1; }\n")
