cmake_minimum_required(VERSION 3.20)
project(custombin C)
# Custom binary dir: sub's CMAKE_CURRENT_BINARY_DIR is <build>/custom/subbuild.
add_subdirectory(sub custom/subbuild)
