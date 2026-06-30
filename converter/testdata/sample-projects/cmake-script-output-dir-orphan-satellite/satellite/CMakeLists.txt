cmake_minimum_required(VERSION 3.20)
project(satellite NONE)   # LANGUAGES NONE — a pure codegen satellite, no targets

# The OUTPUT_DIR consumed-orphan shape, hidden in a `cmake -P` wrapper INSIDE the
# satellite: the ninja-declared OUTPUT is only the manifest stamp, while foo.c /
# foo.h are undeclared side effects the script writes into OUTPUT_DIR — which is
# in the OUTER build tree (OUTER_BUILD). The OUTER app consumes them.
set(GENDIR ${OUTER_BUILD}/gen)
add_custom_command(
  OUTPUT ${GENDIR}/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENDIR}
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${GENDIR}
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh
  VERBATIM)
add_custom_target(gen ALL DEPENDS ${GENDIR}/manifest.cmake)
