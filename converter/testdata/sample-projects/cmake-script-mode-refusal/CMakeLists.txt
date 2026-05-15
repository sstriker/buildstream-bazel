cmake_minimum_required(VERSION 3.20)
project(scriptmode LANGUAGES C VERSION 0.1.0)

# A custom command that invokes cmake in script mode with a cache
# variable assignment BEFORE `-P` — the libpng/pnglibconf shape.
# Pre-PR-#164 the substring detection (`/usr/bin/cmake -P `) missed
# this because the intervening `-DOUTPUT=…` argument broke the
# literal match; the genrule's `cmd` then landed in BUILD.bazel
# pointing at the recording-machine's now-deleted build dir.
#
# Cmake configure succeeds here (script mode is only invoked at
# build time, not at configure time), so record-fileapi.sh can
# capture a real reply. The build itself would fail under
# convert-element-cmake's terms — that's exactly what we're
# asserting: lower.ToIR refuses the recovery with
# `unsupported-custom-command-script`.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/generated.h
    COMMAND ${CMAKE_COMMAND}
        -DOUTPUT=${CMAKE_CURRENT_BINARY_DIR}/generated.h
        -P ${CMAKE_CURRENT_SOURCE_DIR}/scripts/gen.cmake
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/scripts/gen.cmake)

add_library(scriptmode STATIC foo.c
    ${CMAKE_CURRENT_BINARY_DIR}/generated.h)
target_include_directories(scriptmode PRIVATE
    ${CMAKE_CURRENT_BINARY_DIR})
