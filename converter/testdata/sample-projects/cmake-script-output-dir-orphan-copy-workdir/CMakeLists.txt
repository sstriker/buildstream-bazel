cmake_minimum_required(VERSION 3.20)
project(cmake_script_output_dir_orphan_copy_workdir C)

# TEMP-DIR-THEN-COPY OUTPUT_DIR consumed-orphan shape where the SUBPROCESS copy
# runs with the SAME WORKING_DIRECTORY as the generator tool (the temp dir). The
# tool (python3) writes foo.c / foo.h into the temp dir; a `cmake -E
# copy_if_different` — also issued with WORKING_DIRECTORY=<tmp>, so its operands
# are bare basenames resolved in the tempdir — relocates them into OUTPUT_DIR.
#
# Because the copy shares the tool's WORKING_DIRECTORY and passes every tool
# eligibility gate (cmake is liftable, its operands resolve inside the tempdir),
# the temp-dir-relocate tool search would count it as a SECOND producer →
# ambiguous → decline → byte-bake. The recovery must skip relocation calls in
# the tool search (they're already harvested as relocations), leaving the single
# real tool, and extract a regenerating genrule.
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
