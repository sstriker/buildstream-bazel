cmake_minimum_required(VERSION 3.20)
project(protocgrpctwocall CXX)

# The TWO-CALL grpc shape: messages and services compiled in SEPARATE protoc
# invocations for the same svc.proto (the protobuf_generate(LANGUAGE cpp) +
# protobuf_generate(LANGUAGE grpc) pattern).
#
# Call 1 (--cpp_out): svc.pb.{cc,h} -> the cpp recognizer emits
# proto_library(svc_proto) + cc_proto_library(svc_cc_proto).
# Call 2 (--grpc_out only): svc.grpc.pb.{cc,h} -> the grpc-only recognizer emits
# cc_grpc_library(svc_cc_grpc) REFERENCING the sibling :svc_proto/:svc_cc_proto,
# not re-emitting them.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/svc.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/svc.pb.h
    COMMAND protoc --cpp_out=${CMAKE_CURRENT_BINARY_DIR}
                   -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/svc.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/svc.proto VERBATIM)

add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/svc.grpc.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/svc.grpc.pb.h
    COMMAND protoc --grpc_out=${CMAKE_CURRENT_BINARY_DIR}
                   --plugin=protoc-gen-grpc=grpc_cpp_plugin
                   -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/svc.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/svc.proto VERBATIM)

add_custom_target(gen_svc ALL
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/svc.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/svc.grpc.pb.cc)
