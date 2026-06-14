cmake_minimum_required(VERSION 3.20)
project(host_codegen_tool C)

# gen.sh is a project-specific host codegen tool with NO idiomatic native
# Bazel rule. cmake drives it by absolute source path at configure time; the
# recovered genrule must drive the HERMETIC Bazel tool (resolved through the
# imports.json `tools` map) instead of the raw host binary.
add_custom_command(
  OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/greeting.c
  COMMAND ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh
          ${CMAKE_CURRENT_SOURCE_DIR}/greeting.in
          ${CMAKE_CURRENT_BINARY_DIR}/greeting.c
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/greeting.in
          ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh
  COMMENT "generating greeting.c with the host codegen tool")

add_library(greeting STATIC ${CMAKE_CURRENT_BINARY_DIR}/greeting.c)
