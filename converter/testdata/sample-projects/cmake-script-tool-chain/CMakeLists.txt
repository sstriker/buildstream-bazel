cmake_minimum_required(VERSION 3.20)
project(cmake_script_tool_chain C)

# A multi-stage codegen pipeline inside a `cmake -P` wrapper whose INTERMEDIATE
# lives outside the build dir (a system tempdir) — non-anchorable as a Bazel
# artifact. stageA reads input.txt and writes <tmp>/int.tmp; stageB reads
# <tmp>/int.tmp and writes the declared output. The per-step recovery can't
# anchor the /tmp intermediate (it would leak the absolute convert-time path
# into stageB's genrule and never produce it); the chain recovery folds both
# stages into ONE genrule with int.tmp a CWD transient.
#
# Converted with --recognize-codegen --cmake-script-trace.
add_custom_command(
  OUTPUT ${CMAKE_BINARY_DIR}/gen/value.c
  COMMAND ${CMAKE_COMMAND} -E make_directory ${CMAKE_BINARY_DIR}/gen
  COMMAND ${CMAKE_COMMAND}
          -DOUT=${CMAKE_BINARY_DIR}/gen/value.c
          -DA=${CMAKE_CURRENT_SOURCE_DIR}/stageA.py
          -DB=${CMAKE_CURRENT_SOURCE_DIR}/stageB.py
          -DIN=${CMAKE_CURRENT_SOURCE_DIR}/input.txt
          -P ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/gen.cmake
          ${CMAKE_CURRENT_SOURCE_DIR}/stageA.py
          ${CMAKE_CURRENT_SOURCE_DIR}/stageB.py
          ${CMAKE_CURRENT_SOURCE_DIR}/input.txt
  VERBATIM)

add_executable(app main.c ${CMAKE_BINARY_DIR}/gen/value.c)
