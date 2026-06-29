# Temp-dir-relocate: run the tool in a system tempdir (mktemp -d, outside the
# build dir), then relocate its output to the declared OUT via cmake -E copy.
execute_process(COMMAND mktemp -d OUTPUT_VARIABLE _tmp OUTPUT_STRIP_TRAILING_WHITESPACE)
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${_tmp})
execute_process(COMMAND ${CMAKE_COMMAND} -E copy ${_tmp}/value.h ${OUT})
