cmake_minimum_required(VERSION 3.20)
project(foo LANGUAGES CXX VERSION 0.1.0)

# A "header-only library with a dummy compile unit" — the shape
# yasm/libpng-style projects use when they want a STATIC archive
# to exist on disk even though all the code is in headers. cmake
# requires at least one source for STATIC, so projects emit a
# zero-byte placeholder at configure time and feed it into
# add_library() via its absolute build-dir path.
#
# Without #143's fix, lower copied that absolute /tmp/<build>/dummy.cpp
# path verbatim into irt.Srcs and the rendered BUILD.bazel pointed
# at a file that vanished the moment convert-element-cmake's tmp
# build dir was cleaned up. With the fix, the source is elided
# and the consuming target picks up a `cmake-elided-build-dir-source`
# audit tag.
file(WRITE "${CMAKE_BINARY_DIR}/dummy.cpp" "")

add_library(foo STATIC "${CMAKE_BINARY_DIR}/dummy.cpp")
target_include_directories(foo PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
target_sources(foo PUBLIC
    FILE_SET HEADERS
    BASE_DIRS ${CMAKE_CURRENT_SOURCE_DIR}/include
    FILES include/foo/foo.hpp)

install(TARGETS foo
        EXPORT fooTargets
        ARCHIVE DESTINATION lib
        FILE_SET HEADERS DESTINATION include)
install(EXPORT fooTargets
        FILE fooTargets.cmake
        NAMESPACE foo::
        DESTINATION lib/cmake/foo)
