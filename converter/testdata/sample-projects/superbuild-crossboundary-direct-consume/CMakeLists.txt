cmake_minimum_required(VERSION 3.20)
project(superbuild_crossboundary_direct_consume C)

# A nested cmake sub-project whose ONLY target is a UTILITY (add_custom_target)
# running a code generator. The generated SOURCE is written UP into the OUTER
# build tree (OUTER_GEN, outside the nested build) and the OUTER consumes it
# DIRECTLY — no recipe `.cmake` / include() / target_sources() tie (that variant
# is superbuild-crossboundary-cmake-script-recipe).
#
# The nested lowering walks the sub-project's ninja custom command, but the
# output lands in the outer build dir. Without cross-boundary output anchoring
# the standalone recovery leaks the absolute convert-time path (a non-hermetic,
# byte-unstable, illegal Bazel out) and the OUTER bakes a frozen duplicate. The
# fix resolves the escaping output to its OWNING (outer) build-relative form, so
# the sub-project's custom command becomes a regenerating genrule the outer
# consumes — extract over bake.
set(SUBSRC ${CMAKE_CURRENT_SOURCE_DIR}/codegen)
set(SUBBUILD ${CMAKE_CURRENT_BINARY_DIR}/codegen-build)
set(OUTER_GEN ${CMAKE_CURRENT_BINARY_DIR}/generated)

execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${SUBSRC} -B ${SUBBUILD} -G Ninja -DOUTER_GEN_DIR=${OUTER_GEN}
  RESULT_VARIABLE _cfg OUTPUT_QUIET ERROR_QUIET)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build ${SUBBUILD}
  RESULT_VARIABLE _bld OUTPUT_QUIET ERROR_QUIET)

add_executable(app main.c ${OUTER_GEN}/type_a.c)
