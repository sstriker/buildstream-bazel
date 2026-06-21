#!/usr/bin/env python3
"""Cross-boundary codegen stand-in: writes the generated source type_a.c into the
OUTER build tree (argv[1] = OUTER_GEN_DIR, OUTSIDE this nested build dir) and a
STABLE-named recipe types.cmake (argv[2], in the nested build dir) that
target_sources()'s the generated source onto the outer `app` target.

The generated source's home is the cross-boundary axis this fixture exercises:
the recovery must resolve it against the outer build dir, not the nested one."""
import os
import sys

outer_gen = sys.argv[1]
recipe = sys.argv[2]

os.makedirs(outer_gen, exist_ok=True)
gen_src = os.path.join(outer_gen, "type_a.c")
with open(gen_src, "w") as f:
    f.write("int gen_value(void) { return 7; }\n")

with open(recipe, "w") as f:
    f.write("target_sources(app PRIVATE %s)\n" % gen_src.replace("\\", "/"))
