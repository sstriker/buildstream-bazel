cmake_minimum_required(VERSION 3.20)
project(genclass CXX)

# The genclass / template-implementation-include idiom: shape.hpp textually
# #includes its implementation (shape_impl.inl, a non-self-contained inline
# definition, and shape_impl_extra.cc). The impl files are listed on the target
# (so cmake tracks them) but are never compiled standalone — they only make
# sense pasted into shape.hpp's translation context. Under Bazel they must land
# in textual_hdrs, not hdrs (a parse_headers/layering_check build would fail
# compiling the fragment on its own) and not srcs (double-definition).
add_library(shape STATIC
  shape.cpp
  shape.hpp
  shape_impl.inl
  shape_impl_extra.cc)
set_source_files_properties(shape_impl_extra.cc PROPERTIES HEADER_FILE_ONLY TRUE)
target_include_directories(shape PUBLIC ${CMAKE_CURRENT_SOURCE_DIR})
