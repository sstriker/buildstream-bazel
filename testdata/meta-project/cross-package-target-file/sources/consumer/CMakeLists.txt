cmake_minimum_required(VERSION 3.20)
project(consumer LANGUAGES C VERSION 0.1.0)

# Consumer cmake element: depends on `producer` (a kind:cmake
# element). At convert-element time, write-a's cmake handler
# stages producer's synthesized cmake-config bundle under
# $PREFIX/lib/cmake/producer/ and passes --prefix-dir=$PREFIX.
# find_package(producer CONFIG) below resolves against that
# bundle, surfacing `producer::producer` as an IMPORTED static
# library.
find_package(producer CONFIG REQUIRED)

# file(GENERATE) that references $<TARGET_FILE:producer::producer>.
# This is the PR 2 cross-package TARGET_FILE resolution test case:
#
#   - At recording time, cmake expands the genex to the absolute
#     path of producer's archive under the synth-prefix bundle
#     (e.g. <prefix>/lib/libproducer.a). The imports.json
#     manifest written by write-a captures this in LinkPaths.
#   - At convert-element-cmake time, the file(GENERATE) lifter
#     sees the template references an imported target, looks
#     it up in the imports manifest, and emits the lifted
#     genrule with --target-file=producer::producer="$(location
#     //elements/producer:producer)" so Bazel substitutes the
#     producer's actual artifact at action time.
#
# This is the resolved-lift counterpart to PR 1's refusal-stub
# behaviour for unresolvable cross-package references.
file(GENERATE
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/tool_path.h
    CONTENT "#define PRODUCER_ARTIFACT \"$<TARGET_FILE:producer::producer>\"\n")

add_library(consumer STATIC src/consumer.c)
target_include_directories(consumer PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<BUILD_INTERFACE:${CMAKE_CURRENT_BINARY_DIR}>
    $<INSTALL_INTERFACE:include>)
target_link_libraries(consumer PUBLIC producer::producer)

install(TARGETS consumer
        EXPORT consumerTargets
        ARCHIVE DESTINATION lib)
install(DIRECTORY include/ DESTINATION include)
install(EXPORT consumerTargets
        FILE consumerTargets.cmake
        NAMESPACE consumer::
        DESTINATION lib/cmake/consumer)
