add_custom_command(
    OUTPUT ${CMAKE_BINARY_DIR}/pkg/a/a.pb.cc ${CMAKE_BINARY_DIR}/pkg/a/a.pb.h
    COMMAND protoc --cpp_out=${CMAKE_BINARY_DIR} -I ${CMAKE_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/a.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/a.proto VERBATIM)
add_custom_target(gen_a ALL DEPENDS ${CMAKE_BINARY_DIR}/pkg/a/a.pb.cc ${CMAKE_BINARY_DIR}/pkg/a/a.pb.h)
