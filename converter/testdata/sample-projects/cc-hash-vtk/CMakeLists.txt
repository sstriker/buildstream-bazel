cmake_minimum_required(VERSION 3.20)
project(cchashvtk CXX)

# Recognizer fixture: VTK's vtkHashSource.cmake hashes a file into a generated
# header (`#define <NAME> "<digest>"`) at build time via `cmake -P`. With
# --lift-cc-hash the converter recognizes this known hashing script and lowers
# it to the native cc_hash rule (no cmake at build time) instead of refusing
# the cmake -P custom command.
include("${CMAKE_CURRENT_SOURCE_DIR}/vtkHashSource.cmake")

vtk_hash_source(
  INPUT         "data.txt"
  NAME          dataHash
  ALGORITHM     SHA256
  HEADER_OUTPUT data_hash_header)

add_library(cchashvtk STATIC use_hash.cxx "${data_hash_header}")

# The generated header lands in CMAKE_CURRENT_BINARY_DIR; consumers #include it
# by bare name.
target_include_directories(cchashvtk PRIVATE "${CMAKE_CURRENT_BINARY_DIR}")
