# Edge A: run a tool (gen_a.sh) that writes foo.c / foo.h / manifest.cmake into
# OUTPUT_DIR (gen/a). foo.{c,h} are consumed orphans — the ninja edge declares
# only manifest.cmake.
execute_process(
  COMMAND sh ${CMAKE_CURRENT_LIST_DIR}/gen_a.sh ${OUTPUT_DIR}
  RESULT_VARIABLE rv)
if(NOT rv EQUAL 0)
  message(FATAL_ERROR "gen_a.sh failed: ${rv}")
endif()
