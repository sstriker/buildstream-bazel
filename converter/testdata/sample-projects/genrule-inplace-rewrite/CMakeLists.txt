cmake_minimum_required(VERSION 3.20)
project(genruleinplacerewrite C)

# Fixture for the source-tree-input == build-tree-output genrule
# "in-place rewrite" gap (ROADMAP LLVM frontier — the Remarks.exports
# shape). A custom command reads version.txt from the SOURCE tree and
# writes the SAME relative path version.txt into the BUILD tree. After
# conversion both the input and the output anchor to the same package-
# relative path, so the emitted genrule has "version.txt" as BOTH an
# srcs entry and an outs entry — which Bazel rejects at load:
#
#   Error in genrule: rule '...' has file 'version.txt' as both an
#   input and an output
#
# Worse, because the source path and build path collapse to the same
# token, anchorGenruleOutputsToRuledir rewrites BOTH occurrences in the
# cmd to $(RULEDIR)/version.txt, so the recovered cmd copies the output
# onto itself ("cp $(RULEDIR)/version.txt $(RULEDIR)/version.txt") and
# never reads the real source.
#
# See docs/design/genrule-inplace-rewrite.md for the fix design. The
# render gate (to be added with the fix) converts this fixture, asserts
# the output was renamed off the colliding name + the cmd reads the
# source input, and bazel-builds the genrule.

add_library(foo STATIC foo.c)

add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/version.txt
    COMMAND ${CMAKE_COMMAND} -E copy
            ${CMAKE_CURRENT_SOURCE_DIR}/version.txt
            ${CMAKE_CURRENT_BINARY_DIR}/version.txt
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/version.txt
    VERBATIM)
add_custom_target(gen_version ALL DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/version.txt)
