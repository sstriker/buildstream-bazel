cmake_minimum_required(VERSION 3.20)
project(cmake_script_tempdir_relocate_standalone C)

# A STANDALONE cmake -P codegen edge: the output is produced by an
# add_custom_target (no compile target consumes it as a source), so it routes
# through lowerStandaloneCustomCommands -> tryStandaloneCmakeScriptCodegen, NOT
# the per-target recoverGenrule path. The script has a TEMP-DIR-RELOCATE shape
# (tool runs in a mktemp -d, then cmake -E copy relocates the output to the
# declared path) — a wrapper-ladder rung (recoverTempDirToolRelocate).
#
# Before the standalone path ran the full recovery ladder, this fell to the
# per-step recoverExecuteProcess, which FROZE-BAKED the copy destination. With
# the ladder wired in, the regenerating tool genrule is recovered instead.
#
# Converted with --recognize-codegen --cmake-script-trace.
add_custom_command(
  OUTPUT ${CMAKE_BINARY_DIR}/gen/value.h
  COMMAND ${CMAKE_COMMAND} -E make_directory ${CMAKE_BINARY_DIR}/gen
  COMMAND ${CMAKE_COMMAND}
          -DTOOL=${CMAKE_CURRENT_SOURCE_DIR}/tool.py
          -DOUT=${CMAKE_BINARY_DIR}/gen/value.h
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)
add_custom_target(gen ALL DEPENDS ${CMAKE_BINARY_DIR}/gen/value.h)
