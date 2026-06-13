cmake_minimum_required(VERSION 3.24)
project(ewrap C)

# env-WRAPPED generator (cmake -E env K=V <cmd>): the wrapper unwraps
# into the call's ENVIRONMENT and the inner sed hoists with an `env`
# prefix. Historically refused ("cmake -E env is not in the supported
# op set").
execute_process(
  COMMAND ${CMAKE_COMMAND} -E env SUFFIX=gen sed -e "s/@V@/3/"
  INPUT_FILE ${CMAKE_CURRENT_SOURCE_DIR}/val.h.in
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/val.h)

# cmake -E cat concat with OUTPUT_FILE: rewrites to raw `cat` and
# hoists as a concat genrule.
execute_process(
  COMMAND ${CMAKE_COMMAND} -E cat
          ${CMAKE_CURRENT_SOURCE_DIR}/part1.h.in
          ${CMAKE_CURRENT_SOURCE_DIR}/part2.h.in
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/parts.h)

# Console / exit-status forms: benign skips, not refusals.
execute_process(COMMAND ${CMAKE_COMMAND} -E echo "configuring ewrap...")
execute_process(COMMAND ${CMAKE_COMMAND} -E true)

add_executable(app main.c)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR})
