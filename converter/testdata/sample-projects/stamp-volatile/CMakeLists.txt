cmake_minimum_required(VERSION 3.20)
project(stampvolatile C)

# A STABLE_ stamp (git revision) AND a VOLATILE_ stamp (build date) feed one
# configure_file. Exercises the converter's driver-aware stamp lift
# (GIT_SHA -> STABLE_GIT_SHA, BUILD_DATE -> VOLATILE_BUILD_DATE) and, at
# Bazel build time, the cmake_configure_file rule reading BOTH stable-status
# (ctx.info_file) and volatile-status (ctx.version_file) and the tool
# merging them.
find_package(Git REQUIRED)
execute_process(
  COMMAND ${GIT_EXECUTABLE} rev-parse HEAD
  WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
  OUTPUT_VARIABLE GIT_SHA
  OUTPUT_STRIP_TRAILING_WHITESPACE)
execute_process(
  COMMAND date +%Y-%m-%d
  OUTPUT_VARIABLE BUILD_DATE
  OUTPUT_STRIP_TRAILING_WHITESPACE)

configure_file(
  ${CMAKE_CURRENT_SOURCE_DIR}/src/version.h.in
  ${CMAKE_CURRENT_BINARY_DIR}/version.h
  @ONLY)

add_library(stampvolatile STATIC src/version.c)
target_include_directories(stampvolatile PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
