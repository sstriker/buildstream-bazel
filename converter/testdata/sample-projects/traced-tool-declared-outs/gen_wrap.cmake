# User-authored cmake -P wrapper: the real (unrecognized) tool runs HERE, in an
# execute_process, hidden from the ninja edge (which only reads `cmake -P`). The
# converter re-traces this at convert time to recover the gen.sh invocation.
execute_process(
    COMMAND sh ${SRC_DIR}/gen.sh --out-dir=${BIN_DIR} ${SRC_DIR}/greeting.def
    RESULT_VARIABLE _rv)
if(NOT _rv EQUAL 0)
    message(FATAL_ERROR "gen.sh failed (${_rv})")
endif()
