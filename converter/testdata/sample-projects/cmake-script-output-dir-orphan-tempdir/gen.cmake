# Run the tool in a TEMP dir (it writes foo.c / foo.h there), then file(COPY) them
# into OUTPUT_DIR. The manifest stamp is written directly. foo.{c,h} are consumed
# orphans produced via the temp-dir-then-copy wrapper.
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
file(COPY ${TMP}/foo.c DESTINATION ${OUTPUT_DIR})
file(COPY ${TMP}/foo.h DESTINATION ${OUTPUT_DIR})
file(WRITE ${OUTPUT_DIR}/manifest.cmake "# stamp\n")
