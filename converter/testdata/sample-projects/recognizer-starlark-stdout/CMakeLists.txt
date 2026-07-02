cmake_minimum_required(VERSION 3.20)
project(stdoutgen C)

# A STDOUT generator: `sgen` writes generated C to stdout, captured via
# OUTPUT_FILE. It has NO native Bazel rule, so an operator recognizer
# (recognizer.star) lowers it to a genrule redirecting the tool's stdout to $@ —
# exercising the Starlark genrule(...) builtin.
execute_process(
  COMMAND sgen ${CMAKE_CURRENT_SOURCE_DIR}/in.x
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/gen.h)

add_executable(app main.c ${CMAKE_BINARY_DIR}/gen.h)
set_source_files_properties(${CMAKE_BINARY_DIR}/gen.h PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR})
