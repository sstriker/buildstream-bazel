cmake_minimum_required(VERSION 3.20)
project(cmake_script_writeinplace C)

# The WRITE-IN-PLACE codegen shape, hidden inside a `cmake -P` wrapper. The
# custom command runs `cmake -P gen.cmake`, whose script runs a tool with
# WORKING_DIRECTORY == the custom command's DECLARED-output directory. The tool
# writes value.c there BY BASENAME (naming no path in argv) and there is NO
# subsequent relocation (`cmake -E copy`/`rename`) step — its cwd already IS the
# output dir, so the output lands directly at the declared path.
#
# The other recovery rungs can't see this: recoverExecuteProcess /
# recoverTracedToolCommand both need the argv to NAME the output (an operand or a
# `--out=<dir>` flag), and the temp-dir-relocate recovery needs a relocation call
# to harvest. So without the write-in-place recovery this bottoms out at the
# `cmake -P` refusal. The recovery instead recovers the regenerating TOOL: a
# genrule that runs the tool in the genrule cwd and copies its in-place output to
# $(RULEDIR), with the relocation map synthesized from the WORKING_DIRECTORY
# anchor.
#
# Converted with --recognize-codegen --cmake-script-trace. The tool here
# (tool.py) is unrecognized, so it exercises the genrule + lifted-copy path.
add_custom_command(
  OUTPUT ${CMAKE_BINARY_DIR}/gen/value.c
  COMMAND ${CMAKE_COMMAND} -E make_directory ${CMAKE_BINARY_DIR}/gen
  COMMAND ${CMAKE_COMMAND}
          -DOUTDIR=${CMAKE_BINARY_DIR}/gen
          -DTOOL=${CMAKE_CURRENT_SOURCE_DIR}/tool.py
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)

add_executable(app main.c ${CMAKE_BINARY_DIR}/gen/value.c)
