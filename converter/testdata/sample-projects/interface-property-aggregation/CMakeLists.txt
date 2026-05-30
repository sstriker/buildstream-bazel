cmake_minimum_required(VERSION 3.20)
project(ifaceagg LANGUAGES C VERSION 0.1.0)

# Exercises the TARGET_PROPERTY INTERFACE_* convert-time
# aggregation pipeline: a three-level chain of INTERFACE
# libraries propagates an INTERFACE_INCLUDE_DIRECTORIES
# value to the leaf consumer. The convert-time aggregation
# walks codemodel Dependencies[] transitively to mirror
# cmake's own generator-phase property walk.
#
# Chain shape:
#   base (INTERFACE) carries the includes + a define.
#   mid  (INTERFACE) re-exports base via PUBLIC/INTERFACE.
#   leaf (STATIC)    consumes mid; mid's interface props
#                    (including base's, walked transitively)
#                    must surface on leaf via
#                    $<TARGET_PROPERTY:leaf,INTERFACE_*>.
#
# A file(GENERATE) call references leaf's
# INTERFACE_INCLUDE_DIRECTORIES; the lifter's (a) Go-side
# genex evaluator must resolve this to the absolute include
# path at convert time. With the aggregation pipeline
# wired, the genrule's audit tag carries
# `cmake-codegen-genex-resolved`; without
# the aggregation, the evaluator would surface an
# UnsupportedError and the lift would fall back to the
# legacy bytes-embedded shape.
add_library(base INTERFACE)
target_include_directories(base INTERFACE
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>)
target_compile_definitions(base INTERFACE FOO=1)

add_library(mid INTERFACE)
target_link_libraries(mid INTERFACE base)

add_library(leaf STATIC src/leaf.c)
target_link_libraries(leaf PUBLIC mid)

# Reference leaf's transitively-aggregated INTERFACE_INCLUDE_DIRECTORIES
# via file(GENERATE). The (a) evaluator resolves this at convert
# time from the genex Context the lifter builds; the audit tag
# `cmake-codegen-genex-resolved` confirms the
# resolution succeeded.
file(GENERATE
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/leaf_includes.txt
    CONTENT "$<TARGET_PROPERTY:leaf,INTERFACE_INCLUDE_DIRECTORIES>")
