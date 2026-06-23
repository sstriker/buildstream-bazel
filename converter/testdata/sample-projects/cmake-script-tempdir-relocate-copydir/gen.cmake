# Run the tool in a TEMP dir (it writes a.c and b.c there), then relocate the
# whole tree into the declared OUTDIR with `cmake -E copy_directory`. TMP /
# TOOL / OUTDIR arrive as -D args from the custom command.
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(COMMAND ${CMAKE_COMMAND} -E copy_directory ${TMP} ${OUTDIR})
