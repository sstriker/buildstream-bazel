cmake_minimum_required(VERSION 3.20)
project(standalone_buildcopy C)

# A STANDALONE build-dir staging-copy codegen, the grpc shape on the standalone
# path. grpc copies its src/proto/**.proto into <build>/protos/ at CONFIGURE
# time and runs `cd protos && protoc -I . src/proto/x.proto` from there. We
# mirror that with a stand-in tool (gen.py):
#
#  1. At configure time, copy the source-tree input into a build-dir staging dir.
#  2. An add_custom_target (NOT consumed as a compile source -> routes to the
#     STANDALONE genrule pass, not the per-target recovery) runs the tool cd'd
#     INTO that staging dir: `cd <build>/staged && gen.py -I . data/x.txt ...`.
#     So `-I .` and `data/x.txt` are cwd-relative reads of the staged copy.
#
# After cd-strip, those cwd-relative reads point at the empty Bazel exec-root
# cwd. reanchorBuildDirCopyGenrule (now wired into the standalone path, in parity
# with emitRecoveredGenrule) re-anchors them to the byte-identical SOURCE-tree
# inputs the genrule already carries and drops the producerless staged copy.

file(MAKE_DIRECTORY ${CMAKE_BINARY_DIR}/staged/data)
file(COPY ${CMAKE_CURRENT_SOURCE_DIR}/data/x.txt
     DESTINATION ${CMAKE_BINARY_DIR}/staged/data)

add_custom_command(
  OUTPUT ${CMAKE_BINARY_DIR}/gens/x.gen.c
  COMMAND python3 ${CMAKE_CURRENT_SOURCE_DIR}/tools/gen.py
          --out=${CMAKE_BINARY_DIR}/gens -I . data/x.txt
  WORKING_DIRECTORY ${CMAKE_BINARY_DIR}/staged
  DEPENDS ${CMAKE_BINARY_DIR}/staged/data/x.txt
          ${CMAKE_CURRENT_SOURCE_DIR}/data/x.txt
          ${CMAKE_CURRENT_SOURCE_DIR}/tools/gen.py
  VERBATIM)

# add_custom_target wraps the OUTPUT with no compile-source consumer, so the
# edge routes to lowerStandaloneCustomCommands.
add_custom_target(gen_x ALL DEPENDS ${CMAKE_BINARY_DIR}/gens/x.gen.c)
