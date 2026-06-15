cmake_minimum_required(VERSION 3.20)
project(protoccmakescript CXX)

# protoc hidden inside a USER-authored `cmake -P` script (P2 of the wrapper-
# codegen coverage). The ninja edge only sees `cmake -P gen_proto.cmake`; the
# real protoc lives in the script's execute_process, invisible to both the edge
# and the add_custom_command record. With --recognize-codegen --cmake-script-
# trace the converter re-traces the script, recognizes the protoc, and lowers it
# to proto_library + cc_proto_library; rewriteNativeRuleConsumers strips
# foo.pb.cc from use_foo and wires a //:foo_cc_proto deps edge. Without the
# opt-in, the cmake -P custom command is an unrecoverable Tier-1 refusal.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h
    COMMAND ${CMAKE_COMMAND}
        -DPROTOC=protoc
        -DSRC_DIR=${CMAKE_CURRENT_SOURCE_DIR}
        -DBIN_DIR=${CMAKE_CURRENT_BINARY_DIR}
        -P ${CMAKE_CURRENT_SOURCE_DIR}/gen_proto.cmake
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto ${CMAKE_CURRENT_SOURCE_DIR}/gen_proto.cmake
    VERBATIM)

add_library(use_foo STATIC ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc use_foo.cc)
target_include_directories(use_foo PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
