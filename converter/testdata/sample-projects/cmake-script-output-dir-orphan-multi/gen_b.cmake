# Edge B: run a tool (gen_b.sh) that writes bar.c / bar.h / manifest.cmake into
# OUTPUT_DIR (gen/b). bar.{c,h} are consumed orphans — the ninja edge declares
# only manifest.cmake.
execute_process(
  COMMAND sh ${CMAKE_CURRENT_LIST_DIR}/gen_b.sh ${OUTPUT_DIR}
  RESULT_VARIABLE rv)
if(NOT rv EQUAL 0)
  message(FATAL_ERROR "gen_b.sh failed: ${rv}")
endif()
