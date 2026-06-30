cmake_minimum_required(VERSION 3.24)
project(outer C)

# Superbuild-at-configure with a CROSS-BOUNDARY execute_process codegen: the
# sub-project, at CONFIGURE time, runs a tool (execute_process cmake -E copy)
# that writes a generated source UP into the OUTER build tree (OUTER_BUILD),
# not its own CMAKE_CURRENT_BINARY_DIR. The nested sublib consumes that
# cross-boundary source; the OUTER app links sublib. Only the nested trace sees
# the copy, and its output lands in the ancestor build tree — so the recovery
# must re-home both the produced output and the nested consumer's reference to
# the owning OuterBuildDir (the execute_process parity with add_custom_command /
# configure_file / file(GENERATE)).
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/sub
          -B ${CMAKE_BINARY_DIR}/subbuild -G Ninja
          -DOUTER_BUILD=${CMAKE_BINARY_DIR}
  RESULT_VARIABLE _sub_cfg)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build ${CMAKE_BINARY_DIR}/subbuild
  RESULT_VARIABLE _sub_bld)

add_executable(app main.c)
target_link_libraries(app PRIVATE ${CMAKE_BINARY_DIR}/subbuild/libsublib.a)
