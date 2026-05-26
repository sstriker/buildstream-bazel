cmake_minimum_required(VERSION 3.20)
project(standalone_custom_command LANGUAGES C VERSION 0.1.0)

# A regular library — to ensure the rendered BUILD.bazel has at
# least one consumer-style target alongside the standalone edge.
# Nothing about this library depends on the standalone edge below;
# the two coexist in the build graph and the converter must emit
# both.
add_library(stub STATIC src/stub.c)
target_include_directories(stub PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/src)

# The standalone edge: add_custom_command whose output is consumed
# ONLY by an add_custom_target (no cc_library / cc_binary lists
# the output in its sources). The recoverGenrule path won't pick
# it up because no STATIC_LIBRARY / EXECUTABLE / SHARED_LIBRARY
# target references the output; only the Phase 4 standalone walker
# emits a genrule for this shape.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/standalone_stamp.txt
    COMMAND /usr/bin/python3 ${CMAKE_CURRENT_SOURCE_DIR}/scripts/gen_stamp.py ${CMAKE_CURRENT_BINARY_DIR}/standalone_stamp.txt "phase4-graduated"
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/scripts/gen_stamp.py
    VERBATIM
)

# add_custom_target with ALL so cmake adds the edge to the default
# build set; its output isn't a source of any cc target, so it's
# canonically standalone.
add_custom_target(write_stamp ALL
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/standalone_stamp.txt
)
