cmake_minimum_required(VERSION 3.20)
project(cmake_script_tempdir_relocate_copydir C)

# The temp-dir-then-copy codegen shape with the `cmake -E copy_directory`
# relocation form: instead of naming each file, the script relocates a whole
# tempdir TREE into the declared output directory. copy_directory enumerates no
# per-file operand, so the recovery expands it against the edge's DECLARED
# outputs (each declared output under destdir maps to srcdir/<its rel path>).
# The custom command runs `cmake -P gen.cmake`, whose script:
#   1. runs a tool with WORKING_DIRECTORY=<tmp> that writes BOTH a.c and b.c to
#      its cwd, then
#   2. `cmake -E copy_directory <tmp> <declared OUT dir>` relocates the tree to
#      the custom command's DECLARED outputs.
#
# The recovery recovers the regenerating TOOL (here an unrecognized genrule +
# lifted per-file copies) instead of FREEZING each destination's bytes.
#
# Converted with --recognize-codegen --cmake-script-trace.
add_custom_command(
  OUTPUT ${CMAKE_BINARY_DIR}/gen/a.c ${CMAKE_BINARY_DIR}/gen/b.c
  COMMAND ${CMAKE_COMMAND} -E make_directory ${CMAKE_BINARY_DIR}/gen
  COMMAND ${CMAKE_COMMAND}
          -DOUTDIR=${CMAKE_BINARY_DIR}/gen
          -DTOOL=${CMAKE_CURRENT_SOURCE_DIR}/tool.py
          -DTMP=${CMAKE_BINARY_DIR}/_relocate_tmp
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)

add_executable(app main.c ${CMAKE_BINARY_DIR}/gen/a.c ${CMAKE_BINARY_DIR}/gen/b.c)
