# Run the tool in a TEMP dir (it writes value.c there), then `cmake -E copy` it
# to the declared OUT. TMP / TOOL / OUT arrive as -D args from the custom command.
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(COMMAND ${CMAKE_COMMAND} -E copy ${TMP}/value.c ${OUT})
