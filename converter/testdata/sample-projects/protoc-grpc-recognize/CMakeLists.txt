cmake_minimum_required(VERSION 3.20)
project(protocgrpcrecognize CXX)

# A COMBINED protoc invocation: one call generates BOTH the message classes
# (--cpp_out -> svc.pb.{cc,h}) and the C++ gRPC service stubs (--grpc_out via
# the grpc_cpp_plugin -> svc.grpc.pb.{cc,h}) — the common
# protobuf_generate(... PLUGIN grpc) shape. Nothing consumes the generated
# sources, so the converter recovers the edge via the STANDALONE custom-command
# path.
#
# Default: a generic genrule. Under --recognize-codegen the grpc recognizer
# (registered ahead of the cpp one) claims the combined command and lowers it to
# proto_library(svc_proto) + cc_proto_library(svc_cc_proto) +
# cc_grpc_library(svc_cc_grpc, grpc_only = True). It is the OUTPUT AUTHORITY:
# derives svc.pb.{cc,h} + svc.grpc.pb.{cc,h} and cross-checks cmake's recorded
# outputs.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/svc.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/svc.pb.h
           ${CMAKE_CURRENT_BINARY_DIR}/svc.grpc.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/svc.grpc.pb.h
    COMMAND protoc --cpp_out=${CMAKE_CURRENT_BINARY_DIR}
                   --grpc_out=${CMAKE_CURRENT_BINARY_DIR}
                   --plugin=protoc-gen-grpc=grpc_cpp_plugin
                   -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/svc.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/svc.proto
    VERBATIM)

add_custom_target(gen_svc_proto ALL
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/svc.grpc.pb.cc)
