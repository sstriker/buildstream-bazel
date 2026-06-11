cmake_minimum_required(VERSION 3.24)
project(outer C)

# Configure-time nested cmake: bootstrap a sub-project whose artifacts
# the OUTER project consumes (superbuild-at-configure idiom).
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/sub
          -B ${CMAKE_BINARY_DIR}/subbuild -G Ninja -DSUB_VALUE=7
  RESULT_VARIABLE _sub_cfg)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build ${CMAKE_BINARY_DIR}/subbuild
  RESULT_VARIABLE _sub_bld)

add_executable(app main.c)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR}/subbuild)
target_link_libraries(app PRIVATE ${CMAKE_BINARY_DIR}/subbuild/libsublib.a)
