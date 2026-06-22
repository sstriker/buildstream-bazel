# The intermediate lives in a system tempdir (outside the build dir), so it is
# NOT anchorable as a Bazel output — the chain recovery folds the two stages.
execute_process(COMMAND mktemp -d OUTPUT_VARIABLE _tmp OUTPUT_STRIP_TRAILING_WHITESPACE)
execute_process(COMMAND python3 ${A} ${IN} ${_tmp}/int.tmp)
execute_process(COMMAND python3 ${B} ${_tmp}/int.tmp ${OUT})
