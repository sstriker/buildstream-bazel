cmake_minimum_required(VERSION 3.20)
project(ccembedvtk CXX)

# Recognizer fixture: VTK's vtkEncodeString.cmake encodes a data file into a
# C string at build time via `cmake -P`. With --lift-cc-embed the converter
# recognizes this known encoder and lowers it to the native cc_embed rule
# (no cmake at build time) instead of refusing the cmake -P custom command.
include("${CMAKE_CURRENT_SOURCE_DIR}/vtkEncodeString.cmake")

vtk_encode_string(
  INPUT         "shader.glsl"
  NAME          shader_glsl
  HEADER_OUTPUT shader_header
  SOURCE_OUTPUT shader_source)

add_library(ccembedvtk STATIC use_shader.cxx ${shader_source})
