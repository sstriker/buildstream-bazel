# A shared cmake module living OUTSIDE the project source root (this dir is added
# to CMAKE_MODULE_PATH; it is a sibling of the source root, so the trace records
# the execute_process below as issued from out of the source tree). It drives a
# tool on the project's OWN in-tree source (tool.py), writing the generated
# source into the project's OWN build dir.
#
# This is the project's codegen merely ISSUED from an out-of-tree helper: under
# best-effort it must be EXTRACTED to a regenerating genrule, not baked from the
# on-disk bytes (which is what happens when the call is dropped as out-of-tree
# noise).
execute_process(COMMAND ${CMAKE_COMMAND} -E make_directory ${CMAKE_BINARY_DIR}/gen)
execute_process(COMMAND python3 ${CMAKE_SOURCE_DIR}/tool.py ${CMAKE_BINARY_DIR}/gen/value.c)
