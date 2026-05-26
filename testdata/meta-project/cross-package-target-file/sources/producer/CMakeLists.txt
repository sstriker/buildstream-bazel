cmake_minimum_required(VERSION 3.20)
project(producer LANGUAGES C VERSION 0.1.0)

# Producer element: ships a STATIC library + a synthesized
# cmake-config bundle (via convert-element-cmake's cmakecfg
# path) that downstream elements consume via find_package.
# Equivalent to the prod element in testdata/meta-project/cross-cmake/,
# minus the producer-shipped Helpers.cmake (not needed for this gate).

add_library(producer STATIC src/producer.c)
target_include_directories(producer PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)

install(TARGETS producer
        EXPORT producerTargets
        ARCHIVE DESTINATION lib)
install(DIRECTORY include/ DESTINATION include)
install(EXPORT producerTargets
        FILE producerTargets.cmake
        NAMESPACE producer::
        DESTINATION lib/cmake/producer)
