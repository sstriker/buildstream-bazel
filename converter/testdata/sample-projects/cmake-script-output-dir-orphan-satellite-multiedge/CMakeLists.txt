cmake_minimum_required(VERSION 3.20)
project(outer C)

# MULTI-EDGE SHARED-COMPONENT over-attribution: a nested satellite
# (project(LANGUAGES NONE)) has TWO cmake -P custom-command edges writing under a
# shared component dir in the OUTER build tree (OUTER_BUILD/gen/comp):
#   - edge A (gen.cmake): a temp-dir-then-copy TOOL codegen into a HASH subdir
#     (comp/a-h1) — its copy names OUTPUT_DIR=comp/a-h1 as an operand.
#   - edge B (genb.cmake): a file(WRITE) writing a shared header DIRECTLY into the
#     component dir (comp/shared.h).
# The OUTER app consumes comp/a-h1/fa.{c,h} AND comp/shared.h.
#
# The orphan attribution's traced write set adds a dir operand's PARENT
# speculatively, so edge A's copy of OUTPUT_DIR=comp/a-h1 leaks `comp` into edge
# A's write dirs — and comp/shared.h (edge B's, sitting directly under comp) gets
# over-attributed to edge A. Edge A's attributed set then spans two dir levels
# (comp/a-h1 + comp) and the over-attribution guard fires against edge B → edge A
# declines → fa.{c,h} go unrecovered → a hard refusal. Constraining attribution to
# the edge's -D-named OUTPUT_DIR (comp/a-h1) drops the parent leak so edge A owns
# only its hash-subdir codegen and edge B owns comp/shared.h.
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/satellite
          -B ${CMAKE_BINARY_DIR}/satbuild -G Ninja
          -DOUTER_BUILD=${CMAKE_BINARY_DIR})
execute_process(COMMAND ${CMAKE_COMMAND} --build ${CMAKE_BINARY_DIR}/satbuild)

set(COMP ${CMAKE_BINARY_DIR}/gen/comp)
add_executable(app main.c ${COMP}/a-h1/fa.c ${COMP}/a-h1/fa.h ${COMP}/shared.h)
set_source_files_properties(
  ${COMP}/a-h1/fa.c ${COMP}/a-h1/fa.h ${COMP}/shared.h PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${COMP} ${COMP}/a-h1)
