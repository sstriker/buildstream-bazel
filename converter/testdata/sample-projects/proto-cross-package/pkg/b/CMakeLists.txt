add_custom_command(
    OUTPUT ${CMAKE_BINARY_DIR}/pkg/b/b.pb.cc ${CMAKE_BINARY_DIR}/pkg/b/b.pb.h
    COMMAND protoc --cpp_out=${CMAKE_BINARY_DIR} -I ${CMAKE_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/b.proto
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/b.proto VERBATIM)
add_custom_target(gen_b ALL DEPENDS ${CMAKE_BINARY_DIR}/pkg/b/b.pb.cc ${CMAKE_BINARY_DIR}/pkg/b/b.pb.h)
