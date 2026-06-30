cmake_minimum_required(VERSION 3.20)
project(cmake_script_output_dir_orphan_multi C)

# MULTI-EDGE variant of the tool-driven OUTPUT_DIR consumed-orphan shape. TWO
# `cmake -P` edges each run a tool that writes its generated sources into a
# DISTINCT subdir of a shared parent dir (gen/a and gen/b under gen/). Each edge
# declares only its own manifest stamp; foo.{c,h} / bar.{c,h} are consumed
# orphans the app compiles.
#
# This exercises the over-attribution guard's phantom-overlap case: the write-dir
# detection adds both the tool's OUTPUT_DIR operand (gen/a, gen/b) AND its parent
# (gen) to each edge's candidate set, so the shared parent `gen` would be a
# PHANTOM overlap that fires the guard against BOTH edges — declining a perfectly
# unambiguous attribution (the two edges write into disjoint subdirs). The guard
# must compare DEFINITE write targets (the operands, not the speculative parent),
# so gen/a and gen/b don't contend.
set(GENROOT ${CMAKE_BINARY_DIR}/gen)

add_custom_command(
  OUTPUT ${GENROOT}/a/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENROOT}/a
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${GENROOT}/a
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen_a.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen_a.cmake
          ${CMAKE_CURRENT_SOURCE_DIR}/gen_a.sh
  VERBATIM)
add_custom_target(gen_a DEPENDS ${GENROOT}/a/manifest.cmake)

add_custom_command(
  OUTPUT ${GENROOT}/b/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENROOT}/b
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${GENROOT}/b
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen_b.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen_b.cmake
          ${CMAKE_CURRENT_SOURCE_DIR}/gen_b.sh
  VERBATIM)
add_custom_target(gen_b DEPENDS ${GENROOT}/b/manifest.cmake)

add_executable(app
  main.c
  ${GENROOT}/a/foo.c ${GENROOT}/a/foo.h
  ${GENROOT}/b/bar.c ${GENROOT}/b/bar.h)
set_source_files_properties(
  ${GENROOT}/a/foo.c ${GENROOT}/a/foo.h
  ${GENROOT}/b/bar.c ${GENROOT}/b/bar.h
  PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${GENROOT}/a ${GENROOT}/b)
add_dependencies(app gen_a gen_b)
