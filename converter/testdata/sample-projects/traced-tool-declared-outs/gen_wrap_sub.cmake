# Same unrecognized tool, but writing into a build SUBDIR (gen/). Exercises the
# sub-package output-dir widening: the declared OUTPUT is gen/greeting.cpp, so
# the lift anchors --out-dir to $(RULEDIR)/gen (and the split re-relativizes it
# to $(RULEDIR) when the genrule moves into the gen package).
execute_process(
    COMMAND sh ${SRC_DIR}/gen.sh --out-dir=${BIN_DIR}/gen ${SRC_DIR}/greeting.def
    RESULT_VARIABLE _rv)
if(NOT _rv EQUAL 0)
    message(FATAL_ERROR "gen.sh failed (${_rv})")
endif()
