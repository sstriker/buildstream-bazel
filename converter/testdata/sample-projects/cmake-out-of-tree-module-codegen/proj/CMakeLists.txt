cmake_minimum_required(VERSION 3.20)
project(oot_module_codegen C)

# The shared module dir is OUTSIDE this source root (a sibling), reached via
# CMAKE_MODULE_PATH — so include(GenValue) runs an execute_process whose issuing
# `file` is out of the source tree.
list(APPEND CMAKE_MODULE_PATH "${CMAKE_CURRENT_SOURCE_DIR}/../shared-cmake")
include(GenValue)

add_executable(app main.c ${CMAKE_BINARY_DIR}/gen/value.c)
