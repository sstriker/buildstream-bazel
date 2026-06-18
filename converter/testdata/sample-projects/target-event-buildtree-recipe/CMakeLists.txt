cmake_minimum_required(VERSION 3.20)
project(tgteventrecipe C)

add_library(producer STATIC producer.c)

# A recipe generated UNDER THE BUILD TREE at configure time. It declares a
# PRE_LINK stamp command (a build-event hook) that GENERATES gen_impl.c via a
# `>` redirect. The add_custom_command's defining file is the build-tree recipe
# — outside the source root — so the source-tree-relaxed extractor must still
# capture it (a stamp hook is output-bearing; cmake modules / generated recipes
# don't run under Bazel, so the output must be reproduced).
file(WRITE ${CMAKE_BINARY_DIR}/recipe.cmake
"add_custom_command(TARGET producer PRE_LINK COMMAND cat ${CMAKE_SOURCE_DIR}/gen_impl.c.in > ${CMAKE_BINARY_DIR}/gen_impl.c)\n")
include(${CMAKE_BINARY_DIR}/recipe.cmake)

add_library(consumer STATIC consumer.c ${CMAKE_BINARY_DIR}/gen_impl.c)
set_source_files_properties(${CMAKE_BINARY_DIR}/gen_impl.c PROPERTIES GENERATED TRUE)
add_dependencies(consumer producer)
