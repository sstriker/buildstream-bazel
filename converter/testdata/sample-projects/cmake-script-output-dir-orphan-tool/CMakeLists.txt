cmake_minimum_required(VERSION 3.20)
project(cmake_script_output_dir_orphan_tool C)

# TOOL-DRIVEN variant of the OUTPUT_DIR consumed-orphan shape (extract-over-bake).
# Identical wiring to cmake-script-output-dir-orphan, EXCEPT gen.cmake produces the
# orphan sources by running a real TOOL (execute_process over gen.sh) into
# OUTPUT_DIR rather than file(WRITE)ing literal bytes. The OUTPUT_DIR orphan
# recovery should therefore EXTRACT a regenerating genrule that re-runs gen.sh
# (higher fidelity than freezing the bytes), not bake.
set(GENDIR ${CMAKE_BINARY_DIR}/gen)
add_custom_command(
  OUTPUT ${GENDIR}/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENDIR}
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${GENDIR}
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
          ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh
  VERBATIM)
add_custom_target(gen DEPENDS ${GENDIR}/manifest.cmake)

add_executable(app main.c ${GENDIR}/foo.c ${GENDIR}/foo.h)
set_source_files_properties(${GENDIR}/foo.c ${GENDIR}/foo.h PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${GENDIR})
add_dependencies(app gen)
