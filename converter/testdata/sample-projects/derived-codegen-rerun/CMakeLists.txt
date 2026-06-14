cmake_minimum_required(VERSION 3.20)
project(derivedcodegen CXX)

# Derived-name codegen via execute_process: the tool DERIVES its output
# (foo.gen.cc) from the input stem and writes it to its cwd (the build dir) — the
# output is NOT an argv operand, so this is the stem-match recovery. The
# generated .cc is COMPILED into a library (a File-API-demanded source), which is
# what makes it the stem-match path rather than an include-only consumer bake.
# Default: bake the bytes. Under --lift-derived-codegen: re-run as a live genrule.
execute_process(
    COMMAND python3 ${CMAKE_CURRENT_SOURCE_DIR}/gen.py ${CMAKE_CURRENT_SOURCE_DIR}/foo.in)

add_library(use STATIC ${CMAKE_CURRENT_BINARY_DIR}/foo.gen.cc)
