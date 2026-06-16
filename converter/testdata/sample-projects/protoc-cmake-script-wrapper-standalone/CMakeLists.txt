cmake_minimum_required(VERSION 3.20)
project(protoccmakescriptstandalone CXX)

# STANDALONE variant of protoc-cmake-script-wrapper: the script's protoc output
# is driven by an add_custom_TARGET (no cc_library consumes foo.pb.cc as a
# source), so the ninja CUSTOM_COMMAND edge is NOT claimed by the per-target
# recoverGenrule path — it reaches lowerStandaloneCustomCommands. With
# --recognize-codegen --cmake-script-trace the standalone path now re-traces the
# `cmake -P` script (tryStandaloneCmakeScriptCodegen) and lowers the protoc to
# proto_library + cc_proto_library, the same recognize-through-script the
# per-target path already did. Without the opt-in it stays the unrunnable
# `cmake -P` shape (refusal / raw).
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h
    COMMAND ${CMAKE_COMMAND}
        -DPROTOC=protoc
        -DSRC_DIR=${CMAKE_CURRENT_SOURCE_DIR}
        -DBIN_DIR=${CMAKE_CURRENT_BINARY_DIR}
        -P ${CMAKE_CURRENT_SOURCE_DIR}/gen_proto.cmake
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/foo.proto ${CMAKE_CURRENT_SOURCE_DIR}/gen_proto.cmake
    VERBATIM)

# An add_custom_target (not a cc_library source) makes the edge standalone.
add_custom_target(gen_protos ALL
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.cc ${CMAKE_CURRENT_BINARY_DIR}/foo.pb.h)
