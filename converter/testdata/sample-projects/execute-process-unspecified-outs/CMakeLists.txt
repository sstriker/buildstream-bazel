cmake_minimum_required(VERSION 3.20)
project(unspecouts C)

# Dir-operand class: the tool is TOLD where to write (-C <dir>) but the
# output FILES never appear in the argv — only the directory does. The
# converter recovers the outs by enumerating the on-disk files under the
# operand (minus anything ninja produces) and re-runs the tool at Bazel
# time with the operand rewritten to $(RULEDIR)/ext.
file(MAKE_DIRECTORY ${CMAKE_BINARY_DIR}/ext)
execute_process(
  COMMAND tar -xf ${CMAKE_CURRENT_SOURCE_DIR}/payload.tar -C ${CMAKE_BINARY_DIR}/ext)

# Derived-name class: tar extracts data_gen.h into the process cwd (the
# build root) — no output path OR directory in the argv at all. The
# consumed orphan data_gen.h correlates to the input data.tar by stem
# (data + _gen.h), and the configure-written bytes bake.
execute_process(
  COMMAND tar -xf ${CMAKE_CURRENT_SOURCE_DIR}/data.tar)

add_executable(app main.c ${CMAKE_BINARY_DIR}/ext/gen.c ${CMAKE_BINARY_DIR}/data_gen.h)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR})
