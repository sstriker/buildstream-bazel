cmake_minimum_required(VERSION 3.20)
project(outer C)

# CROSS-BOUNDARY dir-operand lift: a nested satellite (project(LANGUAGES NONE))
# runs `tar -xf <archive> -C <dir>` where the extraction DIRECTORY is in the OUTER
# build tree (OUTER_BUILD/ext). The tool is told where to write, but the output
# FILES never appear in the argv — only the dir does. The dir-operand lift detects
# the dir + enumerates its files as outputs; both must consult the OUTER build dir
# (dirUnderBuildRoots), else the dir isn't detected / the walk finds nothing and
# the element falls to not-lifted.
execute_process(
  COMMAND ${CMAKE_COMMAND} -S ${CMAKE_CURRENT_SOURCE_DIR}/satellite
          -B ${CMAKE_BINARY_DIR}/satbuild -G Ninja
          -DOUTER_BUILD=${CMAKE_BINARY_DIR})

set(EXTDIR ${CMAKE_BINARY_DIR}/ext)
add_executable(app main.c ${EXTDIR}/gen.c)
set_source_files_properties(${EXTDIR}/gen.c PROPERTIES GENERATED 1)
target_include_directories(app PRIVATE ${EXTDIR})
