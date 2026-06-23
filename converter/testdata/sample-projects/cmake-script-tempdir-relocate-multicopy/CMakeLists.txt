cmake_minimum_required(VERSION 3.20)
project(cmake_script_tempdir_relocate_multicopy C)

# The temp-dir-then-copy codegen shape with the MULTI-SOURCE `cmake -E copy`
# relocation form: `cmake -E copy <s1> <s2> … <destdir>` copies several files
# INTO a destination directory under their basenames (a single 2-operand copy
# is the cmake-script-tempdir-relocate fixture). The custom command runs
# `cmake -P gen.cmake`, whose script:
#   1. runs a tool with WORKING_DIRECTORY=<tmp> that writes BOTH a.c and b.c to
#      its cwd (naming no final path in argv), then
#   2. `cmake -E copy <tmp>/a.c <tmp>/b.c <declared OUT dir>` relocates both to
#      the custom command's DECLARED outputs in one call.
#
# The temp-dir-relocate recovery must expand the multi-source copy into one
# (src, dst) relocation per source (dst = destdir/basename(src)) so it recovers
# the regenerating TOOL (here an unrecognized genrule + lifted per-file copies)
# instead of FREEZING each destination's bytes.
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
