cmake_minimum_required(VERSION 3.20)
project(cmake_script_tempdir_relocate_rename C)

# The temp-dir-then-copy codegen shape, hidden inside a `cmake -P` wrapper. The
# custom command runs `cmake -P gen.cmake`, whose script:
#   1. runs a tool with WORKING_DIRECTORY=<tmp> (the tool writes value.c to its
#      cwd, naming no final path in argv), then
#   2. `cmake -E copy <tmp>/value.c <declared OUT>` relocates it to the custom
#      command's DECLARED output.
#
# Without the temp-dir-relocate recovery the only lift is FREEZING the copy's
# destination bytes (bakeBuildDirCopyOutput) — a static snapshot. The recovery
# instead recovers the regenerating TOOL (a recognizer's native rule when
# claimed, else a genrule that runs the tool and relocates its output to
# $(RULEDIR)), so the output re-derives when the tool's inputs change.
#
# Converted with --recognize-codegen --cmake-script-trace (the script re-trace
# opt-ins). The tool here (tool.py) is unrecognized, so it exercises the genrule
# + lifted-copy path.
add_custom_command(
  OUTPUT ${CMAKE_BINARY_DIR}/gen/value.c
  COMMAND ${CMAKE_COMMAND} -E make_directory ${CMAKE_BINARY_DIR}/gen
  COMMAND ${CMAKE_COMMAND}
          -DOUT=${CMAKE_BINARY_DIR}/gen/value.c
          -DTOOL=${CMAKE_CURRENT_SOURCE_DIR}/tool.py
          -DTMP=${CMAKE_BINARY_DIR}/_relocate_tmp
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)

add_executable(app main.c ${CMAKE_BINARY_DIR}/gen/value.c)
