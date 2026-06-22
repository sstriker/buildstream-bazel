cmake_minimum_required(VERSION 3.20)
project(codegen C)

# The recipe-producing custom command runs a `cmake -P` script that writes the
# generated source UP into OUTER_GEN_DIR (outside this nested build) and the
# recipe locally.
set(RECIPEDIR ${CMAKE_CURRENT_BINARY_DIR}/recipe)
set(RECIPE ${RECIPEDIR}/types.cmake)

add_custom_command(
  OUTPUT ${RECIPE}
  COMMAND ${CMAKE_COMMAND} -E make_directory ${RECIPEDIR}
  COMMAND ${CMAKE_COMMAND} -E make_directory ${OUTER_GEN_DIR}
  COMMAND ${CMAKE_COMMAND} -DOUTER_GEN_DIR=${OUTER_GEN_DIR} -DRECIPE=${RECIPE} -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  COMMENT "generating cross-boundary codegen recipe via cmake -P"
  VERBATIM)

add_custom_target(gentypes ALL DEPENDS ${RECIPE})
