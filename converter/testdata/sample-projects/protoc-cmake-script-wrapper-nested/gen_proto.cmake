# Outer wrapper: this script does NOT run protoc itself — it shells out to a
# SECOND `cmake -P` script (inner_gen.cmake) via execute_process. That nested
# wrapper is what the P3 recursion driver must descend into: re-tracing this
# script surfaces the `cmake -P inner_gen.cmake` call, which the driver
# recognizes as a wrapper and recurses into, forwarding the same -D cache args.
execute_process(
    COMMAND ${CMAKE_COMMAND}
        -DPROTOC=${PROTOC}
        -DSRC_DIR=${SRC_DIR}
        -DBIN_DIR=${BIN_DIR}
        -P ${SRC_DIR}/inner_gen.cmake
    RESULT_VARIABLE _rv)
if(NOT _rv EQUAL 0)
    message(FATAL_ERROR "inner_gen.cmake failed (${_rv})")
endif()
