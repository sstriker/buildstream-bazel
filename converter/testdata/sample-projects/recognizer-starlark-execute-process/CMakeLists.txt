cmake_minimum_required(VERSION 3.20)
project(recstarexec CXX)

# Configure-time codegen via a non-built-in tool (gen_pb), recognized WITHOUT
# recompiling by an operator Starlark recognizer (recognizer.star). Exercises
# the execute_process recovery path through the Starlark recognizer.
execute_process(
    COMMAND ${CMAKE_CURRENT_SOURCE_DIR}/gen_pb --cpp_out=${CMAKE_CURRENT_BINARY_DIR} -I ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto)

add_library(use_foo STATIC ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc use_foo.cc)
target_include_directories(use_foo PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
