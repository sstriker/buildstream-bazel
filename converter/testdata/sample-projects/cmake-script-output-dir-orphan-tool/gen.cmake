# Produce the REAL generated sources by running a TOOL (gen.sh) into OUTPUT_DIR,
# instead of file(WRITE)ing literal bytes. gen.sh writes foo.c / foo.h /
# manifest.cmake into OUTPUT_DIR. The ninja edge declares only manifest.cmake as
# its OUTPUT, so foo.c / foo.h are consumed orphans with no producer edge — but
# because a tool produces them, the recovery extracts a regenerating genrule.
execute_process(
  COMMAND sh ${CMAKE_CURRENT_LIST_DIR}/gen.sh ${OUTPUT_DIR}
  RESULT_VARIABLE rv)
if(NOT rv EQUAL 0)
  message(FATAL_ERROR "gen.sh failed: ${rv}")
endif()
