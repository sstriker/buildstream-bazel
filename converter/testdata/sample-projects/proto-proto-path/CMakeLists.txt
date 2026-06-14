cmake_minimum_required(VERSION 3.20)
project(protopath CXX)
# protoc with --proto_path = a SUBDIR (not the source root). The protos refer to
# each other by names relative to that root (import "dep.proto"), and protoc's
# output is named relative to it too (dep.pb.cc, not proto/dep.pb.cc).
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/dep.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/dep.pb.h
    COMMAND protoc --proto_path=${CMAKE_CURRENT_SOURCE_DIR}/proto --cpp_out=${CMAKE_CURRENT_BINARY_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/proto/dep.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/proto/dep.proto VERBATIM)
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/main.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/main.pb.h
    COMMAND protoc --proto_path=${CMAKE_CURRENT_SOURCE_DIR}/proto --cpp_out=${CMAKE_CURRENT_BINARY_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/proto/main.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/proto/main.proto VERBATIM)
add_custom_target(gen ALL DEPENDS
    ${CMAKE_CURRENT_BINARY_DIR}/dep.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/main.pb.cc)
