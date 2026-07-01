cmake_minimum_required(VERSION 3.20)
project(satellite NONE)

# Dir-operand execute_process writing into the OUTER build tree: tar -xf ... -C
# <dir> extracts gen.c/gen.h into OUTER_BUILD/ext (the output files are not in the
# argv — only the -C directory is).
set(EXTDIR ${OUTER_BUILD}/ext)
file(MAKE_DIRECTORY ${EXTDIR})
execute_process(COMMAND tar -xf ${CMAKE_CURRENT_SOURCE_DIR}/payload.tar -C ${EXTDIR})
