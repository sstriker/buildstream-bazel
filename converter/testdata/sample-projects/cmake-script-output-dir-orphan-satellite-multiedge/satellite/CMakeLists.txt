cmake_minimum_required(VERSION 3.20)
project(satellite NONE)

set(COMP ${OUTER_BUILD}/gen/comp)

# Edge A: temp-dir-then-copy TOOL codegen into a HASH subdir (comp/a-h1). The copy
# names OUTPUT_DIR=comp/a-h1 as an operand — the parent-expansion leak's source.
add_custom_command(
  OUTPUT ${COMP}/a-h1/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${COMP}/a-h1
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${COMP}/a-h1
          -DTOOL=${CMAKE_CURRENT_SOURCE_DIR}/tool.py
          -DTMP=${OUTER_BUILD}/_tmp_a
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)
add_custom_target(gen_a ALL DEPENDS ${COMP}/a-h1/manifest.cmake)

# Edge B: a sibling edge writing a shared header DIRECTLY into the component dir.
add_custom_command(
  OUTPUT ${COMP}/manifest_b.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${COMP}
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${COMP}
          -P ${CMAKE_CURRENT_SOURCE_DIR}/genb.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/genb.cmake
  VERBATIM)
add_custom_target(gen_b ALL DEPENDS ${COMP}/manifest_b.cmake)
