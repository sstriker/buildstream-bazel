cmake_minimum_required(VERSION 3.20)
cmake_policy(SET CMP0116 NEW)
project(pertarget_depfile_codegen C)

# A PER-TARGET codegen: gen.c is consumed as a source of `app`, so it routes
# through the per-target recovery (recoverGenrule -> emitRecoveredGenrule), NOT
# the standalone path. The custom command declares a DEPFILE, so cmake's Ninja
# generator appends `&& cmake -E cmake_transform_depfile Ninja gccdepfile …`
# pointing at the HOST cmake (absent on the Bazel executor) with absolute build
# paths. The standalone path strips this ninja depfile plumbing; the per-target
# path did not — so the recovered genrule kept an unrunnable segment. With the
# transform parity wired in, emitRecoveredGenrule strips it too.
add_custom_command(
  OUTPUT ${CMAKE_BINARY_DIR}/gen.c
  COMMAND python3 ${CMAKE_CURRENT_SOURCE_DIR}/tool.py ${CMAKE_BINARY_DIR}/gen.c ${CMAKE_BINARY_DIR}/gen.c.d
  DEPFILE ${CMAKE_BINARY_DIR}/gen.c.d
  DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/tool.py
  VERBATIM)

add_executable(app main.c ${CMAKE_BINARY_DIR}/gen.c)
