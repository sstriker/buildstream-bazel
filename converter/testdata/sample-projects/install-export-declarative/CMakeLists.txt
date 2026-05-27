cmake_minimum_required(VERSION 3.23)
project(foopkg LANGUAGES C VERSION 1.0.0)

# Phase 6 generator-parity-uplift fixture: exercises the declarative
# install(EXPORT) classifier + EmitInputs codemodel-only projection.
#
# Layout matches the CMakePackageConfigHelpers canonical shape:
#   install(TARGETS foo bar EXPORT foopkgTargets ...)
#   install(EXPORT foopkgTargets DESTINATION lib/cmake/foopkg)
#
# The classifier must verdict this as Declarative; the EmitInputs
# walk must derive `lib/libfoo.a` + `lib/libbar.so.1` + the bundle
# script at `lib/cmake/foopkg/foopkgTargets.cmake` from codemodel-
# only sources (Target.NameOnDisk + Target.Install.Destinations +
# DirectoryInstaller.Destination); the lowering wires the resulting
# IR into the producer's BUILD.bazel as cc_import / filegroup
# targets (the install(EXPORT) declarative IR projection).

add_library(foo STATIC src/foo.c)
target_include_directories(foo PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)

add_library(bar SHARED src/bar.c)
target_include_directories(bar PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
set_target_properties(bar PROPERTIES VERSION 1.0.0 SOVERSION 1)

install(TARGETS foo bar
        EXPORT foopkgTargets
        ARCHIVE DESTINATION lib
        LIBRARY DESTINATION lib
        RUNTIME DESTINATION bin)
install(DIRECTORY include/ DESTINATION include)
install(EXPORT foopkgTargets
        FILE foopkgTargets.cmake
        NAMESPACE foopkg::
        DESTINATION lib/cmake/foopkg)
