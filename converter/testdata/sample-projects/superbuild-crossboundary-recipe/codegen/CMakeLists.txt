cmake_minimum_required(VERSION 3.20)
project(codegen C)

# OUTER_GEN_DIR (a cache var from the outer configure) is where the OUTER build
# wants the generated sources — OUTSIDE this nested build's own binary dir. The
# custom command declares ONLY the recipe (in this nested build dir) as its
# OUTPUT; the generated source is an undeclared side output written into the
# outer tree (the recipe's target_sources() names it). gen.py is a plain script
# so the recovered genrule is runnable under Bazel (no cmake -P).
set(RECIPEDIR ${CMAKE_CURRENT_BINARY_DIR}/recipe)
set(RECIPE ${RECIPEDIR}/types.cmake)

add_custom_command(
  OUTPUT ${RECIPE}
  COMMAND ${CMAKE_COMMAND} -E make_directory ${RECIPEDIR}
  COMMAND ${CMAKE_COMMAND} -E make_directory ${OUTER_GEN_DIR}
  COMMAND python3 ${CMAKE_CURRENT_SOURCE_DIR}/gen.py ${OUTER_GEN_DIR} ${RECIPE}
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.py
  COMMENT "generating cross-boundary codegen recipe"
  VERBATIM)

add_custom_target(gentypes ALL DEPENDS ${RECIPE})
