cmake_minimum_required(VERSION 3.20)
project(vcsstampindirect C)

# VCS stamp via INDIRECTION: git rev-parse -> GIT_SHA, then a verbatim
# copy `set(VERSION ${GIT_SHA})`, and the configure_file references the
# COPY (@VERSION@) rather than GIT_SHA. This is the Google-Benchmark shape.
# A single --trace-expand pass can't see the copy (expansion erases
# ${GIT_SHA}); the converter's warm second --trace configure recovers it,
# so VERSION inherits GIT_SHA's workspace-status key and the configure_file
# lifts to stamp_values.
find_package(Git REQUIRED)
execute_process(
  COMMAND ${GIT_EXECUTABLE} rev-parse HEAD
  WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
  OUTPUT_VARIABLE GIT_SHA
  OUTPUT_STRIP_TRAILING_WHITESPACE)
set(VERSION ${GIT_SHA})

configure_file(
  ${CMAKE_CURRENT_SOURCE_DIR}/src/version.h.in
  ${CMAKE_CURRENT_BINARY_DIR}/version.h
  @ONLY)

add_library(vcsstampindirect STATIC src/version.c)
target_include_directories(vcsstampindirect PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
