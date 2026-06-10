cmake_minimum_required(VERSION 3.20)
project(percfg C)

# Single-config idiom: a configure_file value derived from CMAKE_BUILD_TYPE
# (the LLVM abi-breaking.h shape).
if(CMAKE_BUILD_TYPE STREQUAL "Debug")
  set(ENABLE_CHECKS 1)
else()
  set(ENABLE_CHECKS 0)
endif()
configure_file(cfg.h.in cfg.h)

add_library(uses STATIC uses.c)
target_include_directories(uses PRIVATE ${CMAKE_BINARY_DIR})
