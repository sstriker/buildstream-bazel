# Run the tool in a TEMP dir (it writes foo.c / foo.h there), then relocate them
# into OUTPUT_DIR with a SUBPROCESS copy (cmake -E copy_if_different, the
# multi-source-to-destination-directory form) — NOT a cmake-native file(COPY).
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(COMMAND ${CMAKE_COMMAND} -E copy_if_different ${TMP}/foo.c ${TMP}/foo.h ${OUTPUT_DIR})
file(WRITE ${OUTPUT_DIR}/manifest.cmake "# stamp\n")
