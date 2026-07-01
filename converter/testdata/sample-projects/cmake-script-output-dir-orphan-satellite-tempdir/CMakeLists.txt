cmake_minimum_required(VERSION 3.20)
project(outer C)

# CROSS-BOUNDARY + TEMP-DIR-THEN-COPY compound: a nested satellite
# (project(LANGUAGES NONE)) runs a tool in a TEMP dir and relocates its output
# into the OUTER build tree (OUTER_BUILD/gen) via `cmake -E copy_if_different`.
# The OUTER app consumes those orphans. The orphan attribution corroborates them
# across the outer build dirs (orphanOnDisk), but the temp-dir-relocate recovery
# then re-corroborated the same declared outputs against the SATELLITE's build
# dir only — missing the cross-boundary orphans → bailing to the byte-bake. The
# fix corroborates declared outputs across the outer build dirs too
# (fileUnderBuildRoots), so the tool EXTRACTS instead of freezing bytes.
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/satellite
          -B ${CMAKE_BINARY_DIR}/satbuild -G Ninja
          -DOUTER_BUILD=${CMAKE_BINARY_DIR})
execute_process(COMMAND ${CMAKE_COMMAND} --build ${CMAKE_BINARY_DIR}/satbuild)

set(GENDIR ${CMAKE_BINARY_DIR}/gen)
add_executable(app main.c ${GENDIR}/foo.c ${GENDIR}/foo.h)
set_source_files_properties(${GENDIR}/foo.c ${GENDIR}/foo.h PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${GENDIR})
