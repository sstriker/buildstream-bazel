cmake_minimum_required(VERSION 3.20)
project(filewritestamp C)

# Configure-time VCS stamp captured into a build-dir header via
# file(WRITE) (no .h.in template — the direct-write idiom). The
# execute_process classifies as a stamp (BucketStamp); the campaign's
# file(WRITE) stamp wiring routes the header through the configure_file
# stamp_values machinery so @GIT_SHA@ re-reads the live revision from
# the Bazel workspace status at build time instead of baking the
# convert-time SHA.
find_package(Git REQUIRED)
execute_process(
  COMMAND ${GIT_EXECUTABLE} rev-parse HEAD
  WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
  OUTPUT_VARIABLE GIT_SHA
  OUTPUT_STRIP_TRAILING_WHITESPACE)

file(WRITE ${CMAKE_CURRENT_BINARY_DIR}/version.h
     "#define GIT_SHA \"${GIT_SHA}\"\n")

add_library(filewritestamp STATIC src/version.c)
target_include_directories(filewritestamp PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
