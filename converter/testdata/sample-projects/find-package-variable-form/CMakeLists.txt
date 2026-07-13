cmake_minimum_required(VERSION 3.20)
project(usepkg_var LANGUAGES C VERSION 0.1.0)

# Variable-form find_package idiom: target_link_libraries consumes
# ${ZLIB_LIBRARIES} instead of the modern namespaced ZLIB::ZLIB
# shape. This is the dominant cmake-external-dep pattern in
# real-world projects (boost, llvm, most pre-3.0-idiom code).
#
# cmake --trace-expand records the expanded arm as the resolved
# absolute path (/usr/lib/x86_64-linux-gnu/libz.so); the lower's
# attributeDirectTraceDeps lifts it via systemLibName to the
# imports manifest's ZLIB entry (link_libraries: ["z"]), emitting
# //elements/zlib as a real dep — a producer claiming the name
# wins over a bare -lz. Without the trace-driven attribution the
# dep dropped silently → BUILD link-failed at Bazel build time.
#
# This fixture pins that path at the live-cmake → codemodel →
# lower → emit boundary so any future rework of the attribution
# pass catches regressions immediately.
find_package(ZLIB REQUIRED)

add_library(usepkg_var SHARED src/usepkg_var.c)
target_include_directories(usepkg_var PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)

# THE TEST: variable-form link AND variable-form include dir.
# Both unconditional ZLIB_LIBRARIES / ZLIB_INCLUDE_DIRS bindings
# from FindZLIB.cmake.
target_link_libraries(usepkg_var PUBLIC ${ZLIB_LIBRARIES})
target_include_directories(usepkg_var PUBLIC ${ZLIB_INCLUDE_DIRS})

set_target_properties(usepkg_var PROPERTIES VERSION 0.1.0 SOVERSION 0)
