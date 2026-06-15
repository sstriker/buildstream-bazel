cmake_minimum_required(VERSION 3.20)
project(protoccmakescriptnested CXX)

# protoc hidden behind TWO nested `cmake -P` wrappers (P3 of the wrapper-codegen
# coverage — the FULL recursion). The ninja edge only sees `cmake -P
# gen_proto.cmake`; that script's execute_process is itself another
# `cmake -P inner_gen.cmake`, and the real protoc lives one level deeper still,
# inside inner_gen.cmake. So unlike P2 (one-level re-trace) this needs the
# recursion driver: re-trace gen_proto, see the nested `cmake -P inner_gen`,
# recurse into inner_gen, and only there bottom out at protoc.
#
# With --recognize-codegen --cmake-script-trace the converter descends both
# levels, recognizes the protoc, and lowers it to proto_library +
# cc_proto_library; rewriteNativeRuleConsumers strips foo.pb.cc from use_foo and
# wires a //:foo_cc_proto deps edge. Without the opt-in it is an unrecoverable
# Tier-1 refusal.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h
    COMMAND ${CMAKE_COMMAND}
        -DPROTOC=protoc
        -DSRC_DIR=${CMAKE_CURRENT_SOURCE_DIR}
        -DBIN_DIR=${CMAKE_CURRENT_BINARY_DIR}
        -P ${CMAKE_CURRENT_SOURCE_DIR}/gen_proto.cmake
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
            ${CMAKE_CURRENT_SOURCE_DIR}/gen_proto.cmake
            ${CMAKE_CURRENT_SOURCE_DIR}/inner_gen.cmake
    VERBATIM)

add_library(use_foo STATIC ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc use_foo.cc)
target_include_directories(use_foo PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
