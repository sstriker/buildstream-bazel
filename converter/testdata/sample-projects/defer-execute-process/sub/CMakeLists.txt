# C: DEFER DIRECTORY to the parent, absolute OUTPUT_FILE.
cmake_language(DEFER DIRECTORY ${CMAKE_SOURCE_DIR} CALL execute_process
  COMMAND cat ${CMAKE_SOURCE_DIR}/tpl_dir.h
  OUTPUT_FILE ${CMAKE_BINARY_DIR}/dirdef.h)
