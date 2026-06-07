cmake_minimum_required(VERSION 3.20)
project(todoscoverage C)

# A wall-clock date stamp written to a file: the converter REFUSES it (a
# non-hermetic stamp driver with OUTPUT_FILE has no faithful Bazel form here).
# In diagnostic mode this surfaces in conversion-todos.json as a
# rejection:<code> entry with disposition "actionable".
execute_process(
  COMMAND date "+%Y"
  OUTPUT_FILE ${CMAKE_CURRENT_BINARY_DIR}/build_year.txt
  OUTPUT_STRIP_TRAILING_WHITESPACE)

add_library(lib STATIC src/lib.c)
