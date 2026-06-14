cmake_minimum_required(VERSION 3.20)
project(tool_convention C)

# A recovered codegen genrule whose driver is a WELL-KNOWN host tool (protoc).
# With no manifest it stays the raw host driver; the host-codegen-tool todo
# names the canonical convention label. Under --tool-conventions the built-in
# registry auto-hermeticizes it to @protobuf//:protoc through the tool-swap.
# (--recognize-codegen is OFF, so protoc is NOT turned into a proto_library; it
# stays a genrule, which is exactly the convention's domain.)
add_custom_command(
  OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/api.gen.c
  COMMAND protoc --plain_c ${CMAKE_CURRENT_SOURCE_DIR}/api.proto
          -o ${CMAKE_CURRENT_BINARY_DIR}/api.gen.c
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/api.proto
  COMMENT "generating api.gen.c with protoc")

add_library(api STATIC ${CMAKE_CURRENT_BINARY_DIR}/api.gen.c)
