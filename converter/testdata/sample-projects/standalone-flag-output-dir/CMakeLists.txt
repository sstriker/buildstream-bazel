cmake_minimum_required(VERSION 3.20)
project(standaloneflagoutdir C)

# An UNRECOGNIZED derived-output codegen tool run DIRECTLY by a custom command
# (no cmake -P wrapper), whose output is consumed via add_custom_target — so it
# lowers through the STANDALONE custom-command path, not the per-target genrule
# path. gen.sh derives greeting.cpp from --out-dir (the filename isn't in the
# argv), so only the output-DIR-flag anchoring (anchorGenruleOutputs) puts it
# under $(RULEDIR). Before that anchoring reached the standalone path, the genrule
# wrote to the exec-root cwd and Bazel failed on the missing declared output.
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/greeting.cpp
    COMMAND sh ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh --out-dir=${CMAKE_CURRENT_BINARY_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/greeting.def
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.sh ${CMAKE_CURRENT_SOURCE_DIR}/greeting.def
    VERBATIM)
add_custom_target(gen_greeting ALL DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/greeting.cpp)
