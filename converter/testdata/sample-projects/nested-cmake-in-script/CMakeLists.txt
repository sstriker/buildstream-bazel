cmake_minimum_required(VERSION 3.24)
project(outer C)

# A nested `cmake -S … -B …` configure+build hidden behind a BUILD-TIME
# `cmake -P` wrapper (the superbuild-at-configure idiom, but deferred to a custom
# command instead of running at the outer configure). The ninja edge only sees
# `cmake -P wrap.cmake`; the nested configure lives inside that script, invisible
# to the MAIN configure trace. With --recognize-codegen --cmake-script-trace the
# converter re-traces the script, records the nested (src,build) pair, and the
# warm second pass + lowerNestedBuilds lift the sub-build: its cc_library merges
# into the outer BUILD, its configure-generated header bakes, and the outer app
# links it — no `cmake -P` genrule, no cmake at Bazel build time. Without the
# opt-in the script stays an (unrunnable) standalone `cmake -P` genrule.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/subbuild/libsublib.a
    COMMAND ${CMAKE_COMMAND}
        -DSRC_DIR=${CMAKE_CURRENT_SOURCE_DIR}
        -DBIN_DIR=${CMAKE_CURRENT_BINARY_DIR}
        -P ${CMAKE_CURRENT_SOURCE_DIR}/wrap.cmake
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/wrap.cmake
            ${CMAKE_CURRENT_SOURCE_DIR}/sub/CMakeLists.txt
            ${CMAKE_CURRENT_SOURCE_DIR}/sub/sub.c
            ${CMAKE_CURRENT_SOURCE_DIR}/sub/sub_config.h.in
    VERBATIM)

add_executable(app main.c)
target_link_libraries(app PRIVATE ${CMAKE_CURRENT_BINARY_DIR}/subbuild/libsublib.a)
add_custom_target(genlib DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/subbuild/libsublib.a)
add_dependencies(app genlib)
