# Run the tool in a TEMP dir (it writes foo.c / foo.h there), then relocate them
# into OUTPUT_DIR with a `cmake -E copy_if_different` ALSO issued with
# WORKING_DIRECTORY=<tmp> — so the copy shares the tool's working dir (the
# ambiguity trigger the relocation-skip guards against).
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(
  COMMAND ${CMAKE_COMMAND} -E copy_if_different foo.c foo.h ${OUTPUT_DIR}
  WORKING_DIRECTORY ${TMP})
file(WRITE ${OUTPUT_DIR}/manifest.cmake "# stamp\n")
