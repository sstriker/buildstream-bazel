cmake_minimum_required(VERSION 3.20)
project(superbuild_crossboundary_recipe C)

# Superbuild-at-configure with CROSS-BOUNDARY output placement. A nested codegen
# project is configured + built during the outer configure (execute_process).
# Unlike the co-located case (superbuild-hashed-recipe, where the generated
# source lands in the nested build dir), here the nested build writes its
# generated SOURCES UP into the OUTER build tree (OUTER_GEN, passed in as a cache
# var) while only the recipe .cmake lands in the nested build dir.
#
# That cross-boundary placement is the point: the nested-recipe recovery
# relativizes a recipe's generated sources against the NESTED build dir, so a
# source that escapes it (into the outer build tree) was dropped and byte-baked
# as a write_file. The recovery must instead resolve the escaping source against
# the OUTER build dir that owns it and declare it on a regenerating genrule.
#
# The recipe filename is STABLE here (no per-configure hash) to isolate the
# cross-boundary axis from the hash-instability one (superbuild-hashed-recipe
# covers that).
set(SUBSRC ${CMAKE_CURRENT_SOURCE_DIR}/codegen)
set(SUBBUILD ${CMAKE_CURRENT_BINARY_DIR}/codegen-build)
set(OUTER_GEN ${CMAKE_CURRENT_BINARY_DIR}/generated)

execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${SUBSRC} -B ${SUBBUILD} -G Ninja -DOUTER_GEN_DIR=${OUTER_GEN}
  RESULT_VARIABLE _cfg
  OUTPUT_QUIET ERROR_QUIET)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build ${SUBBUILD}
  RESULT_VARIABLE _bld
  OUTPUT_QUIET ERROR_QUIET)

add_executable(app main.c)

# Include the recipe the nested build produced; its target_sources() adds the
# generated source (which lives in OUTER_GEN, OUTSIDE the nested build) to `app`.
include(${SUBBUILD}/recipe/types.cmake)
