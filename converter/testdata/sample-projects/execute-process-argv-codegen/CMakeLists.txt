cmake_minimum_required(VERSION 3.20)
project(argvcodegen C)

# Positional + flag operands: sort -o <out> <in>. The output path appears
# in the argv (no OUTPUT_FILE redirect), so only the argv-codegen lift can
# recover it.
execute_process(
  COMMAND sort -o ${CMAKE_BINARY_DIR}/sorted.txt ${CMAKE_CURRENT_SOURCE_DIR}/words.txt)

# Keyed operands: dd if=/of=.
execute_process(
  COMMAND dd if=${CMAKE_CURRENT_SOURCE_DIR}/raw.bin of=${CMAKE_BINARY_DIR}/copy.bin)

# Chain: the second call consumes the first call's argv-declared output.
execute_process(
  COMMAND sort -r -o ${CMAKE_BINARY_DIR}/reversed.txt ${CMAKE_BINARY_DIR}/sorted.txt)
