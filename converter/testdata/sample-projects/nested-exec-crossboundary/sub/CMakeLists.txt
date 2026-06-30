cmake_minimum_required(VERSION 3.20)
project(subproj C)
# CROSS-BOUNDARY (immediate): a configure-time tool writes a generated source UP
# into the OUTER build tree (OUTER_BUILD); THIS nested lib consumes it from there.
execute_process(
  COMMAND ${CMAKE_COMMAND} -E copy
          ${CMAKE_CURRENT_SOURCE_DIR}/gen_cross.c.in
          ${OUTER_BUILD}/gen_cross.c)
# CROSS-BOUNDARY (DEFERRED): the same shape via cmake_language(DEFER CALL …),
# scheduled for end-of-scope but still executed at configure time — the trace
# captures it and the recovery handles it identically.
cmake_language(DEFER CALL execute_process
  COMMAND ${CMAKE_COMMAND} -E copy
          ${CMAKE_CURRENT_SOURCE_DIR}/gen_deferred.c.in
          ${OUTER_BUILD}/gen_deferred.c)
add_library(sublib STATIC sub.c
  ${OUTER_BUILD}/gen_cross.c
  ${OUTER_BUILD}/gen_deferred.c)
