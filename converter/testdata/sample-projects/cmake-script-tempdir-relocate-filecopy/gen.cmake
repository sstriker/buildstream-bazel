# Run the tool in a TEMP dir (it writes value.c there), then `file(COPY)` — a
# cmake command, not an execute_process — it to the declared output's directory.
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
get_filename_component(_outdir ${OUT} DIRECTORY)
file(COPY ${TMP}/value.c DESTINATION ${_outdir})
