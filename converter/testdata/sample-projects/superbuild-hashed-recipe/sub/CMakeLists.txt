cmake_minimum_required(VERSION 3.20)
project(sub C)

# A cached, per-configure-incrementing counter — models a codegen whose recipe
# filename carries a hash that is NOT stable across cmake configures. The outer's
# execute_process build records one value; the converter's nested traced
# re-configure records a later one, so the recipe's ninja-output name diverges
# from the name in the outer trace (the gap the recovery upgrade must bridge).
if(NOT DEFINED GEN_COUNTER)
  set(GEN_COUNTER 0 CACHE INTERNAL "codegen recipe counter")
else()
  math(EXPR _next "${GEN_COUNTER}+1")
  set(GEN_COUNTER ${_next} CACHE INTERNAL "codegen recipe counter")
endif()

set(GENDIR ${CMAKE_CURRENT_BINARY_DIR}/gen)
set(RECIPE ${GENDIR}/recipe-${GEN_COUNTER}.cmake)

# The command declares ONLY the recipe as its OUTPUT; gen_src.c is an undeclared
# side output (the recipe's target_sources() names it). gen.py is a plain script
# so the recovered genrule is runnable under Bazel (no cmake -P).
add_custom_command(
  OUTPUT ${RECIPE}
  COMMAND ${CMAKE_COMMAND} -E make_directory ${GENDIR}
  COMMAND python3 ${CMAKE_CURRENT_SOURCE_DIR}/gen.py ${GENDIR} ${GEN_COUNTER}
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.py
  COMMENT "generating codegen recipe ${GEN_COUNTER}"
  VERBATIM)

add_custom_target(genrecipe ALL DEPENDS ${RECIPE})
