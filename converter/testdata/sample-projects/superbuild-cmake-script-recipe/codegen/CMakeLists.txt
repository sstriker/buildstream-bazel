cmake_minimum_required(VERSION 3.20)
project(codegen C)

# The recipe-producing custom command runs a `cmake -P` SCRIPT (gen.cmake). The
# command declares ONLY the recipe as its OUTPUT; the generated source type_a.c
# is an undeclared side output the recipe's target_sources() names.
set(GENDIR ${CMAKE_CURRENT_BINARY_DIR}/gen)
set(RECIPE ${GENDIR}/recipe.cmake)

add_custom_command(
  OUTPUT ${RECIPE}
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENDIR}
  COMMAND ${CMAKE_COMMAND} -DGENDIR=${GENDIR} -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  COMMENT "generating codegen recipe via cmake -P"
  VERBATIM)

add_custom_target(gentypes ALL DEPENDS ${RECIPE})
