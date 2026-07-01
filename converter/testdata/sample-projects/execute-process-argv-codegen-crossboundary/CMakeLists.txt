cmake_minimum_required(VERSION 3.20)
project(outer C)

# CROSS-BOUNDARY argv-codegen: a nested satellite (project(LANGUAGES NONE)) runs a
# configure-time execute_process whose tool NAMES its output in argv (sort -o),
# writing a generated source into the OUTER build tree (OUTER_BUILD/gen). The
# OUTER app consumes it. The argv-codegen lift corroborates the argv-named output
# on disk before claiming it; that corroboration must walk the OUTER build dir
# (fileUnderBuildRoots) — stat'ing the satellite's own build dir alone misses the
# cross-boundary output and the lift declines (element falls to not-lifted).
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/satellite
          -B ${CMAKE_BINARY_DIR}/satbuild -G Ninja
          -DOUTER_BUILD=${CMAKE_BINARY_DIR})

set(GENDIR ${CMAKE_BINARY_DIR}/gen)
add_executable(app main.c ${GENDIR}/foo.c)
set_source_files_properties(${GENDIR}/foo.c PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${GENDIR})
