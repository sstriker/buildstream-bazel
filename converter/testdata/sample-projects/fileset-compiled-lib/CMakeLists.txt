cmake_minimum_required(VERSION 3.23)
project(fscl LANGUAGES CXX)

# A COMPILED library (has srcs) that also exports public headers via
# FILE_SET HEADERS — the modern-cmake pattern Phase 7 slice 2 targets.
add_library(fscl src/fscl.cpp)
target_sources(fscl PUBLIC
    FILE_SET HEADERS
    BASE_DIRS ${CMAKE_CURRENT_SOURCE_DIR}/include
    FILES include/fscl/fscl.hpp)
target_include_directories(fscl PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)

add_executable(fscl_demo src/demo.cpp)
target_link_libraries(fscl_demo PRIVATE fscl)
