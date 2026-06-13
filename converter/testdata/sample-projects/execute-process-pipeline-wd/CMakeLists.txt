cmake_minimum_required(VERSION 3.24)
project(pipewd C)

# Multi-COMMAND pipeline (cmake chains stage stdout like a shell pipe):
# historically refused flat; lifts as `( cat ... | sort ) > out`.
execute_process(
  COMMAND cat ${CMAKE_CURRENT_SOURCE_DIR}/defs.txt
  COMMAND sort
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/sorted.h)

# WORKING_DIRECTORY moved off the build root + a RELATIVE OUTPUT_FILE
# (cmake resolves it against the cwd): historically refused; lifts with
# the exec-root-save prologue and the out anchored under work/.
execute_process(
  COMMAND sed -e "s/@N@/7/"
  INPUT_FILE ${CMAKE_CURRENT_SOURCE_DIR}/num.h.in
  OUTPUT_FILE out/num.h
  WORKING_DIRECTORY ${CMAKE_BINARY_DIR}/work)

add_executable(app main.c)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR} ${CMAKE_BINARY_DIR}/work/out)
