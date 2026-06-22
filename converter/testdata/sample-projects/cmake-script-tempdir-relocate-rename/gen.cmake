# Run the tool in a TEMP dir (it writes value.c there), then `cmake -E rename` —
# an atomic MOVE (the write-to-tempfile-then-rename idiom) — to the declared
# output. rename maps identically to copy for recovery: the genrule re-runs the
# tool and copies its cwd output to $(RULEDIR). (Unlike copy, rename doesn't
# create its destination dir; the recovery pre-creates the declared outputs'
# parent dirs before the standalone re-trace so the rename completes.)
file(MAKE_DIRECTORY ${TMP})
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${TMP})
execute_process(COMMAND ${CMAKE_COMMAND} -E rename ${TMP}/value.c ${OUT})
