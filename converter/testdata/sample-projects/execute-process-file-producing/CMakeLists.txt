cmake_minimum_required(VERSION 3.20)
project(execute_process_file_producing C)

find_package(Python3 COMPONENTS Interpreter REQUIRED)

# Run a generator script at configure time and redirect its
# stdout into a build-tree header. The converter's bucket
# classifier flags this as BucketFileProducing (OUTPUT_FILE
# declared); the lifter hoists it to a build-time genrule
# tagged cmake-codegen-execute-process-hoisted, with
# scripts/gen.py and spec.txt as Bazel-tracked srcs.
execute_process(
    COMMAND ${Python3_EXECUTABLE}
        ${CMAKE_CURRENT_SOURCE_DIR}/scripts/gen.py
        ${CMAKE_CURRENT_SOURCE_DIR}/spec.txt
    OUTPUT_FILE ${CMAKE_CURRENT_BINARY_DIR}/generated.h
)

add_library(thelib STATIC src/lib.c)
target_include_directories(thelib PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
