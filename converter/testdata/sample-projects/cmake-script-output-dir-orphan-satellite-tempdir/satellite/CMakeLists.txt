cmake_minimum_required(VERSION 3.20)
project(satellite NONE)

# The tool runs in a TEMP dir and its output is relocated into OUTPUT_DIR, which
# is in the OUTER build tree (OUTER_BUILD/gen) — the cross-boundary temp-dir
# shape. The ninja edge declares only the manifest stamp; foo.{c,h} are the
# cross-boundary consumed orphans.
set(GENDIR ${OUTER_BUILD}/gen)
add_custom_command(
  OUTPUT ${GENDIR}/manifest.cmake
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENDIR}
  COMMAND ${CMAKE_COMMAND}
          -DOUTPUT_DIR=${GENDIR}
          -DTOOL=${CMAKE_CURRENT_SOURCE_DIR}/tool.py
          -DTMP=${OUTER_BUILD}/_relocate_tmp
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)
add_custom_target(gen ALL DEPENDS ${GENDIR}/manifest.cmake)
