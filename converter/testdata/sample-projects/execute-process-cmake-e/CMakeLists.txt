cmake_minimum_required(VERSION 3.20)
project(execute_process_cmake_e C)

# Stage a build-tree marker via cmake -E touch. The converter's
# trace dispatcher classifies this as BucketCMakeE; the lifter
# emits a Bazel genrule with cmd="touch $@", outs=[marker.stamp],
# and the cmake-codegen-execute-process-op=touch tag. No tool
# label is needed at Bazel time — touch is a plain shell builtin
# and cmake -E touch's semantics map cleanly to it.
execute_process(
    COMMAND ${CMAKE_COMMAND} -E touch ${CMAKE_BINARY_DIR}/marker.stamp
)

add_library(thelib STATIC src/lib.c)
