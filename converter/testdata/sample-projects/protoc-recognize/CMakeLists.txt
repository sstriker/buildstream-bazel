cmake_minimum_required(VERSION 3.20)
project(protocrecognize CXX)

# A protoc --cpp_out custom command: foo.proto -> foo.pb.{cc,h}. Nothing in
# this project consumes the generated sources, so the converter recovers the
# edge via the STANDALONE custom-command path.
#
# By default that path emits a generic (hermetic) genrule. Under the opt-in
# --recognize-codegen flag, the codegen-recognizer registry recognizes the
# protoc --cpp_out invocation by its DRIVER + flags and lowers it to the
# idiomatic native rule pair instead: proto_library(foo_proto) +
# cc_proto_library(foo_cc_proto). The recognizer is the OUTPUT AUTHORITY — it
# derives foo.pb.{cc,h} from the .proto basename and cross-checks against the
# outputs cmake recorded here.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h
    COMMAND protoc --cpp_out=${CMAKE_CURRENT_BINARY_DIR} -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto
    VERBATIM)

add_custom_target(gen_foo_proto ALL
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h)
