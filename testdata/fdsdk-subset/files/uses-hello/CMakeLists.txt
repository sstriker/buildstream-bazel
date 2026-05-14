cmake_minimum_required(VERSION 3.20)
project(uses_hello LANGUAGES C VERSION 0.1.0)

# Cross-element dependency on the orchestrator fixture's `hello` element.
# At convert time the orchestrator stages hello's synthesized bundle into
# a per-element CMAKE_PREFIX_PATH tree (with zero-byte stubs at the
# IMPORTED_LOCATION paths the bundle references), so find_package
# resolves and target_link_libraries(... hello::hello) records the dep
# in the codemodel. Lower then translates hello::hello to
# @elem_components_hello//:hello via the per-element imports manifest
# the orchestrator writes.
find_package(hello CONFIG REQUIRED)

add_library(uses_hello STATIC src/uses_hello.c)
target_include_directories(uses_hello PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
target_link_libraries(uses_hello PUBLIC hello::hello)

# Static libs don't record link deps in the codemodel — interface
# propagation only fires at consumer time. The orchestrator's e2e
# acceptance gate needs to see the cross-element dep land on a
# real link line, so this executable depends on both.
add_executable(uses_hello_bin src/uses_hello_bin.c)
target_link_libraries(uses_hello_bin PRIVATE uses_hello hello::hello)

install(TARGETS uses_hello
        EXPORT uses_helloTargets
        ARCHIVE DESTINATION lib)
install(EXPORT uses_helloTargets
        FILE uses_helloTargets.cmake
        NAMESPACE uses_hello::
        DESTINATION lib/cmake/uses_hello)
# Mark the executable as installed too so the converter emits
# visibility = ["//visibility:public"] on it. Bazel 9 enforces
# visibility strictly; the M5 downstream-Bazel test depends on
# @elem_components_uses_hello//:uses_hello_bin from //:smoke and
# the dep would be rejected as private without the install hint.
install(TARGETS uses_hello_bin RUNTIME DESTINATION bin)
