cmake_minimum_required(VERSION 3.20)
project(satellite NONE)

# Configure-time argv-codegen execute_process writing into the OUTER build tree:
# `sort -o <out> <in>` names its output in argv, so only the argv-codegen lift can
# recover it, and the output (gen/foo.c) lives cross-boundary under OUTER_BUILD.
set(GENDIR ${OUTER_BUILD}/gen)
file(MAKE_DIRECTORY ${GENDIR})
execute_process(COMMAND sort -o ${GENDIR}/foo.c ${CMAKE_CURRENT_SOURCE_DIR}/foo.c.in)
