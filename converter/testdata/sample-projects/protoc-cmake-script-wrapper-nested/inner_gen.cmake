# Inner wrapper: the real protoc invocation finally lives HERE, one level below
# gen_proto.cmake. The recursion bottoms out at this leaf execute_process — the
# driver routes it through the execute_process codegen recognizer, which lowers
# it to proto_library + cc_proto_library and corroborates foo.pb.{cc,h} against
# the on-disk artifacts this run writes to BIN_DIR.
execute_process(
    COMMAND ${PROTOC} --cpp_out=${BIN_DIR} -I ${SRC_DIR} ${SRC_DIR}/foo.proto
    RESULT_VARIABLE _rv)
if(NOT _rv EQUAL 0)
    message(FATAL_ERROR "protoc failed (${_rv})")
endif()
