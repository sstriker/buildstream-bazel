# Run the tool in a TEMP dir (it writes a.c and b.c there), then relocate BOTH
# into the declared OUTDIR with a single multi-source `cmake -E copy`. TMP /
# TOOL / OUTDIR arrive as -D args from the custom command.
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(COMMAND ${CMAKE_COMMAND} -E copy ${TMP}/a.c ${TMP}/b.c ${OUTDIR})
