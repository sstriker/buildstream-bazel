cmake_minimum_required(VERSION 3.20)
project(superbuild_cmake_script_recipe C)

# Superbuild-at-configure where the nested codegen recipe is produced by a
# `cmake -P` SCRIPT (not a plain python/shell tool). The nested build's UTILITY
# runs `cmake -P gen.cmake`, which file(WRITE)s both the generated source
# (type_a.c) and the recipe (recipe.cmake) that target_sources()'s it onto the
# outer `app`.
#
# The point: the recipe edge's command is cmake-script mode, so the recovery
# diverts to the cmake-script path — which must still honor the recipe's declared
# generated sources (under --cmake-script-bake) so type_a.c is recovered as a
# recipe-attributed producer, NOT left to the consumer-side build-dir bake. The
# converter is run with --cmake-script-bake by the gate (running cmake -P at
# convert time is opt-in; it has no -P dependency at Bazel BUILD time).
set(SUBSRC ${CMAKE_CURRENT_SOURCE_DIR}/codegen)
set(SUBBUILD ${CMAKE_CURRENT_BINARY_DIR}/codegen-build)

execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${SUBSRC} -B ${SUBBUILD} -G Ninja
  RESULT_VARIABLE _cfg
  OUTPUT_QUIET ERROR_QUIET)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build ${SUBBUILD}
  RESULT_VARIABLE _bld
  OUTPUT_QUIET ERROR_QUIET)

add_executable(app main.c)

# Include the recipe the nested cmake -P script produced; its target_sources()
# adds the generated source to `app`.
include(${SUBBUILD}/gen/recipe.cmake)
