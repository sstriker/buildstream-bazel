#!/usr/bin/env python3
# Writes the generated source AND a (trivial) gcc-style depfile, the shape that
# makes cmake's Ninja generator wrap the command with cmake -E cmake_transform_depfile.
import sys

with open(sys.argv[1], "w") as f:
    f.write("int gen_value(void) { return 7; }\n")
with open(sys.argv[2], "w") as f:
    f.write("%s: %s\n" % (sys.argv[1], sys.argv[0]))
