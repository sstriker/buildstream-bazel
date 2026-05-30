cmake_minimum_required(VERSION 3.24)
project(genexprobe LANGUAGES C VERSION 0.1.0)

# Phase 3 generator-parity render fixture (ROADMAP.md). The
# file(GENERATE) below embeds a $<TARGET_OBJECTS:obj> generator
# expression in its CONTENT. internal/genexeval's (a) Go-side
# evaluator REFUSES this offline (UnsupportedError: TARGET_OBJECTS
# with empty Objects — the Go evaluator has no object list without
# cmake's generation-phase answer). With --probe-genex the cmake
# probe hook resolves $<TARGET_OBJECTS:obj> at generation time and
# feeds the resolved value into the lift Context, so the converter
# resolves the genex (cmake-codegen-genex-resolved) instead of
# falling back to the bytes-embedded unresolved shape.
add_library(obj OBJECT src/a.c)
target_include_directories(obj PUBLIC include)

# A real consumer so cmake keeps the OBJECT_LIBRARY's objects in
# the generated build graph.
add_library(consumer STATIC src/main.c $<TARGET_OBJECTS:obj>)

# The genex-bearing file(GENERATE). The objects list cmake
# resolves here is the load-bearing wire the probe captures.
file(GENERATE
    OUTPUT "${CMAKE_BINARY_DIR}/obj_manifest.txt"
    CONTENT "objects=$<TARGET_OBJECTS:obj>\n")
