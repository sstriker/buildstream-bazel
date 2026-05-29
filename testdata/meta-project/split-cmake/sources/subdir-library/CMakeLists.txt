cmake_minimum_required(VERSION 3.20)
project(toplib LANGUAGES C VERSION 0.1.0)

# Top-level CMakeLists pulls in a sub-CMakeLists via add_subdirectory.
# Drives the --split-packages multi-package emit end-to-end: the
# codemodel reply lists targets defined across files; the converter
# emits one BUILD.bazel per directory (root toplib, src/util/util) plus
# a synthesized include/ header library so cmake's single-`-I`-root
# include semantics survive the package split.
add_subdirectory(src/util)

add_library(toplib STATIC src/toplib.c)
target_include_directories(toplib PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
target_link_libraries(toplib PUBLIC util)

install(TARGETS toplib util
        EXPORT toplibTargets
        ARCHIVE DESTINATION lib)
install(DIRECTORY include/ DESTINATION include)
install(EXPORT toplibTargets
        FILE toplibTargets.cmake
        NAMESPACE toplib::
        DESTINATION lib/cmake/toplib)
