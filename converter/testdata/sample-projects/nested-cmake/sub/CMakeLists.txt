cmake_minimum_required(VERSION 3.20)
project(subproj C)
configure_file(sub_config.h.in sub_config.h)
# A nested execute_process codegen: only the nested TRACE can classify
# and lift this (the codemodel just shows a build-dir source).
execute_process(
  COMMAND ${CMAKE_COMMAND} -E copy
          ${CMAKE_CURRENT_SOURCE_DIR}/sub_extra.c.in
          ${CMAKE_CURRENT_BINARY_DIR}/sub_extra.c)
add_library(sublib STATIC sub.c ${CMAKE_CURRENT_BINARY_DIR}/sub_extra.c)
target_include_directories(sublib PUBLIC ${CMAKE_CURRENT_BINARY_DIR})
