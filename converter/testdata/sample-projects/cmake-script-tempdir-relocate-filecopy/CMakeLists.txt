cmake_minimum_required(VERSION 3.20)
project(cmake_script_tempdir_relocate_filecopy C)

# The temp-dir-then-copy codegen shape where the relocation is a `file(COPY)`
# (a cmake command, NOT an execute_process). The custom command runs
# `cmake -P gen.cmake`, whose script runs a tool with WORKING_DIRECTORY=<tmp>
# (writing value.c there) and then `file(COPY <tmp>/value.c DESTINATION <dir>)`
# to the declared output's directory.
#
# Because file(COPY) is a cmake command, it is harvested from the script trace as
# a file-WRITER (not among the execute_process calls), then folded into the same
# relocation map the `cmake -E copy` form uses — so the regenerating tool is
# recovered (here a genrule that runs the tool and relocates its output to
# $(RULEDIR)) instead of the copy's destination bytes being frozen.
#
# Converted with --recognize-codegen --cmake-script-trace.
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
