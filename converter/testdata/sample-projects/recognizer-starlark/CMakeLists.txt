cmake_minimum_required(VERSION 3.20)
project(recognizerstarlark CXX)

# A project-specific protobuf compiler wrapper `gen_pb` the converter has NO
# built-in recognizer for. An operator teaches the converter about it WITHOUT
# recompiling, via a Starlark recognizer (recognizer.star) loaded with
# --recognizers. The recognizer maps `gen_pb --cpp_out` to proto_library +
# cc_proto_library, exactly like the built-in protoc recognizer would — proving
# the operator-extension path end to end.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h
    COMMAND gen_pb --cpp_out=${CMAKE_CURRENT_BINARY_DIR} -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    VERBATIM)

add_custom_target(gen_foo_proto
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h)

add_library(use_foo STATIC use_foo.cc)
add_dependencies(use_foo gen_foo_proto)
target_include_directories(use_foo PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
