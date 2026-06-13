cmake_minimum_required(VERSION 3.20)
project(protocconsumer CXX)

# Same protoc --cpp_out custom command as the protoc-recognize fixture, but
# here a cc_library CONSUMES the generated header: use_foo.cc #includes
# foo.pb.h and depends on the codegen target. This exercises the consumer-dep
# generalization — under --recognize-codegen the consumer must get a DIRECT
# deps edge to the cc_proto_library (:foo_cc_proto), NOT the file-oriented
# generated_includes textual_hdrs wrapper (the header is produced inside the
# cc_proto_library, not as a standalone genrule output).
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h
    COMMAND protoc --cpp_out=${CMAKE_CURRENT_BINARY_DIR} -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    VERBATIM)

add_custom_target(gen_foo_proto
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h)

# use_foo #includes the generated header; add_dependencies records the codegen
# edge the converter walks to attribute the generated header to this consumer.
add_library(use_foo STATIC use_foo.cc)
add_dependencies(use_foo gen_foo_proto)
target_include_directories(use_foo PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
