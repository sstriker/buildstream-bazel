# Run the tool in a TEMP dir, then relocate its output into OUTPUT_DIR (the OUTER
# build tree) with a `cmake -E copy_if_different`. foo.{c,h} are cross-boundary
# consumed orphans the OUTER app compiles.
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(COMMAND ${CMAKE_COMMAND} -E copy_if_different ${TMP}/foo.c ${TMP}/foo.h ${OUTPUT_DIR})
file(WRITE ${OUTPUT_DIR}/manifest.cmake "# stamp\n")
