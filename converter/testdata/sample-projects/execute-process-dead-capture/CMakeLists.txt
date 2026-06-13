cmake_minimum_required(VERSION 3.24)
project(deadcap C)

# 1. SILENCED nested cmake (captures exist purely to quiet the child's
#    console output; the variables are never read) — historically the
#    capture refused the entire nested-build lift.
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/sub
          -B ${CMAKE_BINARY_DIR}/subbuild -G Ninja
  OUTPUT_VARIABLE _sub_out
  ERROR_VARIABLE _sub_err)
execute_process(COMMAND ${CMAKE_COMMAND} --build ${CMAKE_BINARY_DIR}/subbuild)

# 2. File-producing call exercising the upgraded keyword gates: stdin
#    from a source template (INPUT_FILE), stderr to a build-dir log
#    (ERROR_FILE), and a configure-time watchdog (TIMEOUT).
execute_process(
  COMMAND sed -e "s/@WHO@/world/"
  INPUT_FILE ${CMAKE_CURRENT_SOURCE_DIR}/greeting.h.in
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/greeting.h
  ERROR_FILE ${CMAKE_BINARY_DIR}/greeting.err
  TIMEOUT 30)

# 3. Dead-captured stamp: the value is never read — must SKIP (nothing
#    consumes it), not refuse.
execute_process(COMMAND date +%Y OUTPUT_VARIABLE _now_unused)

add_executable(app main.c)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR})
target_link_libraries(app PRIVATE ${CMAKE_BINARY_DIR}/subbuild/libsublib.a)
