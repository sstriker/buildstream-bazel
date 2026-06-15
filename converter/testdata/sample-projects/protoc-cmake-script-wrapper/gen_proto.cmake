# A user-authored cmake -P wrapper: the real protoc invocation lives HERE, in an
# execute_process, hidden from the ninja edge (which only reads `cmake -P`). P2
# re-traces this script at convert time to recover it.
execute_process(
    COMMAND ${PROTOC} --cpp_out=${BIN_DIR} -I ${SRC_DIR} ${SRC_DIR}/foo.proto
    RESULT_VARIABLE _rv)
if(NOT _rv EQUAL 0)
    message(FATAL_ERROR "protoc failed (${_rv})")
endif()
