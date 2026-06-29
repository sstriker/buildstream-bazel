cmake_minimum_required(VERSION 3.20)
project(build_tree_recipe_file_copy C)

# Finding G5 [13]: a file(COPY) ISSUED from a generated+include()d recipe
# `.cmake` that lives in the BUILD tree (not the source tree). We synthesize the
# recipe at configure time, then include() it from the build dir so the
# file(COPY) below is recorded as issued from /<build>/recipe.cmake.
#
# Historically ExtractFileWriterCalls gated on inSourceTree, so a writer issued
# from the build tree was dropped to a frozen on-disk byte-bake. The fix gates
# on inProjectScope (build-tree-aware), mirroring the sibling configure_file /
# file(GENERATE) extractors, so the copy is recovered as a regenerating
# cp-genrule that re-runs at Bazel build time and tracks edits to the committed
# source.
file(WRITE ${CMAKE_BINARY_DIR}/recipe.cmake
  "file(COPY \${CMAKE_CURRENT_SOURCE_DIR}/payload.c DESTINATION \${CMAKE_BINARY_DIR}/copied)\n")
include(${CMAKE_BINARY_DIR}/recipe.cmake)

# The copied build-dir source is the target's only definition of payload(); the
# binary links only if the recipe-issued copy was recovered.
add_executable(app main.c ${CMAKE_BINARY_DIR}/copied/payload.c)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR})
