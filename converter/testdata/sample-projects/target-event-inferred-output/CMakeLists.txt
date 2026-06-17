cmake_minimum_required(VERSION 3.20)
project(tgteventinfer C)

# A target with a PRE_LINK build-event command that GENERATES a source file but
# declares NO BYPRODUCTS — the output is named only by a `>` redirect on the
# command line. cmake folds the command onto producer's link edge (it isn't a
# CUSTOM_COMMAND, so recoverGenrule can't reach it) and, with no BYPRODUCTS, the
# command would be dropped as a pure side-effect. lowerTargetEventCommands instead
# INFERS the output from the redirect target (best-effort) and synthesizes a
# genrule so the consumer below resolves it.
add_library(producer STATIC producer.c)
add_custom_command(TARGET producer PRE_LINK
  COMMAND cat ${CMAKE_CURRENT_SOURCE_DIR}/gen_impl.c.in > ${CMAKE_CURRENT_BINARY_DIR}/gen_impl.c
  COMMENT "generate gen_impl.c before link (no BYPRODUCTS; output inferred from >)")

# consumer compiles the inferred-generated source as one of its sources.
add_library(consumer STATIC consumer.c ${CMAKE_CURRENT_BINARY_DIR}/gen_impl.c)
set_source_files_properties(${CMAKE_CURRENT_BINARY_DIR}/gen_impl.c PROPERTIES GENERATED TRUE)
add_dependencies(consumer producer)
