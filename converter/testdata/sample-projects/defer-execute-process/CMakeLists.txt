cmake_minimum_required(VERSION 3.20)
project(deferep2 C)

# A: regular file-producing hoist (control).
execute_process(
  COMMAND cat ${CMAKE_SOURCE_DIR}/tpl_reg.h
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/reg.h)

# B: same shape, plain DEFER.
cmake_language(DEFER CALL execute_process
  COMMAND cat ${CMAKE_SOURCE_DIR}/tpl_def.h
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/def.h)

add_subdirectory(sub)

add_library(uses STATIC uses.c)
target_include_directories(uses PRIVATE ${CMAKE_BINARY_DIR})
