cmake_minimum_required(VERSION 3.20)
project(superbuild_hashed_recipe C)

# Superbuild-at-configure: configure + BUILD a nested cmake project during the
# outer configure (execute_process), then consume its outputs. The nested build
# emits a codegen "recipe" .cmake whose filename carries a per-configure-unstable
# hash (modeled by a cached counter in sub/), plus a generated source the recipe
# pulls into the outer target via target_sources(). The generated source is an
# UNDECLARED side output of the recipe-producing command, so it reaches the
# converter's build-dir bake fallback today; the nested-recipe recovery upgrade
# should instead wire it to a regenerating genrule.
set(SUBSRC ${CMAKE_CURRENT_SOURCE_DIR}/sub)
set(SUBBUILD ${CMAKE_CURRENT_BINARY_DIR}/subbuild)

execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${SUBSRC} -B ${SUBBUILD} -G Ninja
  RESULT_VARIABLE _cfg
  OUTPUT_QUIET ERROR_QUIET)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build ${SUBBUILD}
  RESULT_VARIABLE _bld
  OUTPUT_QUIET ERROR_QUIET)

add_executable(app main.c)

# Discover the hash-named recipe the nested build produced and include it; the
# recipe's target_sources() adds the generated source to `app`.
file(GLOB _recipes ${SUBBUILD}/gen/recipe-*.cmake)
include(${_recipes})
