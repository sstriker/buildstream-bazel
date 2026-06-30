cmake_minimum_required(VERSION 3.20)
project(cmake_script_output_dir_orphan_tempdir C)

# TEMP-DIR-THEN-COPY variant of the OUTPUT_DIR consumed-orphan shape. The
# `cmake -P` wrapper runs a tool in a TEMP dir (WORKING_DIRECTORY=<tmp>, so its
# argv names the tempdir, NOT OUTPUT_DIR) and then file(COPY)s the tool's output
# into OUTPUT_DIR. The ninja edge declares only the manifest stamp; foo.{c,h} are
# consumed orphans the app compiles.
#
# The direct-write tool extract can't see the producer (the tool's argv doesn't
# name OUTPUT_DIR — the relocation carries the mapping). The recovery must keep
# the relocations and reuse recoverTempDirToolRelocate, extracting a regenerating
# tool genrule that re-runs the tool and relocates its output to $(RULEDIR) —
# NOT freezing the copied bytes.
set(GENDIR ${CMAKE_BINARY_DIR}/gen)
add_custom_command(
  OUTPUT ${GENDIR}/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENDIR}
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${GENDIR}
          -DTOOL=${CMAKE_CURRENT_SOURCE_DIR}/tool.py
          -DTMP=${CMAKE_BINARY_DIR}/_relocate_tmp
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
          ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)
add_custom_target(gen DEPENDS ${GENDIR}/manifest.cmake)

add_executable(app main.c ${GENDIR}/foo.c ${GENDIR}/foo.h)
set_source_files_properties(${GENDIR}/foo.c ${GENDIR}/foo.h PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${GENDIR})
add_dependencies(app gen)
