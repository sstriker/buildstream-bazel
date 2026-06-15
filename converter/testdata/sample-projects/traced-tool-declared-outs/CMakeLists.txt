cmake_minimum_required(VERSION 3.20)
project(tracedtooldeclaredouts CXX)

# An UNRECOGNIZED codegen tool hidden inside a `cmake -P` wrapper, recovered as a
# direct-tool genrule from the custom command's DECLARED output. The real tool
# (gen.sh) derives its output filename from an --out-dir flag, so the argv-output
# lift can't read it and no recognizer claims it — but the add_custom_command
# OUTPUT clause names greeting.cpp, which is enough data to lift: with
# --recognize-codegen --cmake-script-trace the converter re-traces the wrapper,
# sees `sh gen.sh --out-dir=<bin> greeting.def`, and emits a genrule that runs
# gen.sh directly with --out-dir rewritten to $(RULEDIR) and greeting.def staged
# as a src. No --cmake-script-runner is involved — we run the real tool, not
# `cmake -P`. Without the opt-in the cmake -P custom command is an unrecoverable
# Tier-1 refusal.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/greeting.cpp
    COMMAND ${CMAKE_COMMAND}
        -DSRC_DIR=${CMAKE_CURRENT_SOURCE_DIR}
        -DBIN_DIR=${CMAKE_CURRENT_BINARY_DIR}
        -P ${CMAKE_CURRENT_SOURCE_DIR}/gen_wrap.cmake
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/greeting.def
            ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh
            ${CMAKE_CURRENT_SOURCE_DIR}/gen_wrap.cmake
    VERBATIM)

add_library(use_greeting STATIC ${CMAKE_CURRENT_BINARY_DIR}/greeting.cpp use_greeting.cc)
target_include_directories(use_greeting PRIVATE ${CMAKE_CURRENT_BINARY_DIR})

# Sub-package output-dir variant (widening): the tool writes into gen/ under the
# build dir, so the declared output is gen/greeting.cpp.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/gen/greeting.cpp
    COMMAND ${CMAKE_COMMAND}
        -DSRC_DIR=${CMAKE_CURRENT_SOURCE_DIR}
        -DBIN_DIR=${CMAKE_CURRENT_BINARY_DIR}
        -P ${CMAKE_CURRENT_SOURCE_DIR}/gen_wrap_sub.cmake
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/greeting.def
            ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh
            ${CMAKE_CURRENT_SOURCE_DIR}/gen_wrap_sub.cmake
    VERBATIM)

add_library(use_greeting_sub STATIC ${CMAKE_CURRENT_BINARY_DIR}/gen/greeting.cpp use_greeting_sub.cc)
target_include_directories(use_greeting_sub PRIVATE ${CMAKE_CURRENT_BINARY_DIR}/gen)
