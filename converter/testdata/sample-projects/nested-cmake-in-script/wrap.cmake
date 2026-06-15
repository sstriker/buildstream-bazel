# The build-time wrapper: a nested cmake configure + build. Only re-tracing this
# script at convert time surfaces the nested (src,build) pair for the lift.
execute_process(
    COMMAND ${CMAKE_COMMAND} -S ${SRC_DIR}/sub -B ${BIN_DIR}/subbuild -G Ninja -DSUB_VALUE=42
    RESULT_VARIABLE _cfg)
execute_process(
    COMMAND ${CMAKE_COMMAND} --build ${BIN_DIR}/subbuild
    RESULT_VARIABLE _bld)
if(NOT _cfg EQUAL 0 OR NOT _bld EQUAL 0)
    message(FATAL_ERROR "nested configure/build failed")
endif()
