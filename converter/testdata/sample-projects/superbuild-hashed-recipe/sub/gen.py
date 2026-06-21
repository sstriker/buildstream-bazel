#!/usr/bin/env python3
"""Codegen stand-in: writes the generated source gen_src.c (stable name) and a
recipe-<counter>.cmake that target_sources()'s it into the outer `app` target.

The recipe carries the unstable counter in its filename; the generated source
does not. Old recipes are removed so exactly one exists on disk (the 1:1 the
recovery's directory remap relies on)."""
import glob
import os
import sys

gendir = sys.argv[1]
counter = sys.argv[2]

os.makedirs(gendir, exist_ok=True)
for old in glob.glob(os.path.join(gendir, "recipe-*.cmake")):
    os.remove(old)

gen_src = os.path.join(gendir, "gen_src.c")
with open(gen_src, "w") as f:
    f.write("int gen_value(void) { return 42; }\n")

recipe = os.path.join(gendir, "recipe-%s.cmake" % counter)
with open(recipe, "w") as f:
    f.write("target_sources(app PRIVATE %s)\n" % gen_src.replace("\\", "/"))
