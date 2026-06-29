cmake_minimum_required(VERSION 3.20)
project(codegen C)

# UTILITY-only sub-project: the custom command runs an in-tree tool (gen.py) that
# writes the generated source UP into OUTER_GEN_DIR (outside this nested build).
add_custom_command(
  OUTPUT ${OUTER_GEN_DIR}/type_a.c
  COMMAND ${CMAKE_COMMAND} -E make_directory ${OUTER_GEN_DIR}
  COMMAND python3 ${CMAKE_CURRENT_SOURCE_DIR}/gen.py ${OUTER_GEN_DIR}/type_a.c
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.py
  COMMENT "cross-boundary codegen into the outer build tree"
  VERBATIM)
add_custom_target(gentypes ALL DEPENDS ${OUTER_GEN_DIR}/type_a.c)
