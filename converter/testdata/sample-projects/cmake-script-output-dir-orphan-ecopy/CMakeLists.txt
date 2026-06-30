cmake_minimum_required(VERSION 3.20)
project(cmake_script_output_dir_orphan_ecopy C)

# TEMP-DIR-THEN-COPY OUTPUT_DIR consumed-orphan shape where the copy is a
# SUBPROCESS — execute_process(COMMAND cmake -E copy_if_different <srcs…>
# <OUTPUT_DIR>) — not a cmake-native file(COPY). The tool (python3) writes foo.c
# / foo.h into a temp dir; the subprocess copy relocates them into OUTPUT_DIR.
# The ninja edge declares only the manifest stamp; foo.{c,h} are consumed orphans.
#
# The `cmake -E copy_if_different … ${OUTPUT_DIR}` call NAMES the orphans' dir as
# its destination, so the direct-write tool scan would mis-pick the RELOCATION as
# the generator and emit a broken genrule (`cp _tmp/foo.c … $(RULEDIR)/gen`) that
# copies a tempdir file nothing regenerates. The recovery must treat the
# `cmake -E copy` subprocess as a relocation and recover the real python tool
# behind it (a regenerating genrule that runs the tool and relocates to $(RULEDIR)).
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
