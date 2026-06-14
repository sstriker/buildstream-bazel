cmake_minimum_required(VERSION 3.20)
project(protocexecconsumer CXX)

# Configure-time protoc, then a library that COMPILES the generated foo.pb.cc
# (lists it as a source) and includes foo.pb.h. Under --recognize-codegen the
# execute_process lowers to proto_library + cc_proto_library, and use_foo has
# the generated foo.pb.cc stripped from srcs + a deps edge to :foo_cc_proto.
execute_process(
    COMMAND protoc --cpp_out=${CMAKE_CURRENT_BINARY_DIR} -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto)

add_library(use_foo STATIC ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc use_foo.cc)
target_include_directories(use_foo PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
