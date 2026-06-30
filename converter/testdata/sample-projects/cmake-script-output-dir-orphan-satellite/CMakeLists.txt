cmake_minimum_required(VERSION 3.20)
project(outer C)

# A NESTED SATELLITE sub-project (project(LANGUAGES NONE) — no compiler, a pure
# code-generation UTILITY) owns the `cmake -P` custom command that writes the
# OUTPUT_DIR consumed orphans into the OUTER build tree (OUTER_BUILD). The OUTER
# project's app CONSUMES those orphan sources. Producer (satellite cmake -P edge)
# and consumer (outer app) are split across the nested boundary — the
# cross-boundary OUTPUT_DIR-orphan shape. Neither lowering's orphan pass can
# connect them alone: the satellite has the edge but no demand (project(NONE)
# consumes nothing); the outer has the demand but the edge lives in the
# satellite's ninja graph.
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/satellite
          -B ${CMAKE_BINARY_DIR}/satbuild -G Ninja
          -DOUTER_BUILD=${CMAKE_BINARY_DIR}
  RESULT_VARIABLE _sat_cfg)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build ${CMAKE_BINARY_DIR}/satbuild
  RESULT_VARIABLE _sat_bld)

set(GENDIR ${CMAKE_BINARY_DIR}/gen)
add_executable(app main.c ${GENDIR}/foo.c ${GENDIR}/foo.h)
set_source_files_properties(${GENDIR}/foo.c ${GENDIR}/foo.h PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${GENDIR})
