cmake_minimum_required(VERSION 3.20)
project(cmake_script_output_dir_orphan C)

# The OUTPUT_DIR consumed-orphan codegen shape, hidden inside a `cmake -P`
# wrapper. The custom command's ninja-declared OUTPUT is ONLY a `.cmake`
# stamp/manifest (manifest.cmake), while the REAL generated sources (foo.c /
# foo.h) are an UNDECLARED side effect: gen.cmake file(WRITE)s them into the
# directory passed as `-DOUTPUT_DIR=<dir>`. The executable consumes
# ${GENDIR}/foo.c (and lists foo.h), but cmake/ninja has no producer edge for
# either — only the stamp — so they are consumed ORPHANS.
#
# The argv-named-output rungs (recoverExecuteProcess / recoverTracedToolCommand /
# write-in-place) all key on execute_process leaf calls or the edge's declared
# outs (= the stamp only), so they can't see the file(WRITE) side effects. Without
# the OUTPUT_DIR orphan recovery the consumer bottoms out at the `cmake -P`
# refusal (Tier-1, EXIT 65) on gen/foo.c.
#
# Converted with --recognize-codegen --cmake-script-trace. The recovery re-traces
# the script, detects the write directory from where the traced file(WRITE)s land,
# attributes the consumed orphans under it (corroborated on disk), and emits a
# regenerating producer declaring gen/foo.c (+ gen/foo.h + the stamp).
set(GENDIR ${CMAKE_BINARY_DIR}/gen)
add_custom_command(
  OUTPUT ${GENDIR}/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENDIR}
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${GENDIR}
          -DMANIFEST=${GENDIR}/manifest.cmake
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  VERBATIM)
add_custom_target(gen DEPENDS ${GENDIR}/manifest.cmake)

add_executable(app main.c ${GENDIR}/foo.c ${GENDIR}/foo.h)
set_source_files_properties(${GENDIR}/foo.c ${GENDIR}/foo.h PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${GENDIR})
add_dependencies(app gen)
