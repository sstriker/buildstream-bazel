cmake_minimum_required(VERSION 3.20)
project(vcsstamp C)

# Configure-time VCS stamp: capture the git revision into a header a
# library target compiles in. This is the canonical pattern the
# converter's stamp lift maps onto Bazel's --stamp /
# --workspace_status_command mechanism: the execute_process classifies as
# a stamp (BucketStamp), and the configure_file consuming @GIT_SHA@ lifts
# to a cmake_configure_file rule whose stamp_values re-reads the live
# revision from the workspace status at build time.
find_package(Git REQUIRED)
execute_process(
  COMMAND ${GIT_EXECUTABLE} rev-parse HEAD
  WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
  OUTPUT_VARIABLE GIT_SHA
  OUTPUT_STRIP_TRAILING_WHITESPACE)

configure_file(
  ${CMAKE_CURRENT_SOURCE_DIR}/src/version.h.in
  ${CMAKE_CURRENT_BINARY_DIR}/version.h
  @ONLY)

add_library(vcsstamp STATIC src/version.c)
target_include_directories(vcsstamp PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
