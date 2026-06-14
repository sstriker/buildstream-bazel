cmake_minimum_required(VERSION 3.20)
project(protoccmdsrc CXX)

# protoc via add_custom_command whose generated foo.pb.cc is COMPILED as a
# source of use_foo (the per-target recoverGenrule recovery path, not the
# standalone one). Under --recognize-codegen this lowers to proto_library +
# cc_proto_library; rewriteNativeRuleConsumers strips foo.pb.cc from use_foo
# and wires a deps edge to :foo_cc_proto.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h
    COMMAND protoc --cpp_out=${CMAKE_CURRENT_BINARY_DIR} -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    VERBATIM)

add_library(use_foo STATIC ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc use_foo.cc)
target_include_directories(use_foo PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
