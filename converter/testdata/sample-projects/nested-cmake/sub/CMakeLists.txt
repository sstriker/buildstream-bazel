cmake_minimum_required(VERSION 3.20)
project(subproj C)
configure_file(sub_config.h.in sub_config.h)
# A nested execute_process codegen: only the nested TRACE can classify
# and lift this (the codemodel just shows a build-dir source).
execute_process(
  COMMAND ${CMAKE_COMMAND} -E copy
          ${CMAKE_CURRENT_SOURCE_DIR}/sub_extra.c.in
          ${CMAKE_CURRENT_BINARY_DIR}/sub_extra.c)
# Depth-2 nested cmake (the superbuild CHAIN): this sub-project
# bootstraps its OWN sub-sub project at configure time. Liftable only
# by the driver-side worklist - the traced re-configure of THIS dir
# surfaces the grandchild configure, and the grandchild dir is then
# staged and traced directly.
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/subsub
          -B ${CMAKE_BINARY_DIR}/subsubbuild -G Ninja -DSUBSUB_VALUE=11
  RESULT_VARIABLE _subsub_cfg)
add_library(sublib STATIC sub.c ${CMAKE_CURRENT_BINARY_DIR}/sub_extra.c)
target_include_directories(sublib PUBLIC ${CMAKE_CURRENT_BINARY_DIR})
# sub.c includes the GRANDCHILD's configure-generated header.
target_include_directories(sublib PRIVATE ${CMAKE_BINARY_DIR}/subsubbuild)
