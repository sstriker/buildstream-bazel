# Produce the generated sources by running a TOOL (gen.sh) into OUTPUT_DIR (which
# is the OUTER build tree). foo.c / foo.h are consumed orphans — the satellite's
# ninja edge declares only the manifest stamp.
execute_process(
  COMMAND sh ${CMAKE_CURRENT_LIST_DIR}/gen.sh ${OUTPUT_DIR}
  RESULT_VARIABLE rv)
if(NOT rv EQUAL 0)
  message(FATAL_ERROR "gen.sh failed: ${rv}")
endif()
