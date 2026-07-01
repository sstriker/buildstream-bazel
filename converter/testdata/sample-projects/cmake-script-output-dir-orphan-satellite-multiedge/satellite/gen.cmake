# Run the tool in a TEMP dir, then relocate its output into OUTPUT_DIR (a HASH
# subdir under the shared component dir in the OUTER build tree) with a
# `cmake -E copy_if_different`. fa.{c,h} are cross-boundary consumed orphans.
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(COMMAND ${CMAKE_COMMAND} -E copy_if_different ${TMP}/fa.c ${TMP}/fa.h ${OUTPUT_DIR})
file(WRITE ${OUTPUT_DIR}/manifest.cmake "# stamp a\n")
