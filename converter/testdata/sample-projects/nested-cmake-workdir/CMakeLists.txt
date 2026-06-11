cmake_minimum_required(VERSION 3.24)
project(outerwd C)

# Configure-time nested cmake via the WORKING_DIRECTORY + positional-source
# idiom (the dominant "download/build at configure" shape, e.g.
# cryptoauthlib's mbedtls downloader, the googletest DownloadProject trick):
# the sub-build runs IN <build>/subbuild, configured by a POSITIONAL source
# dir (no -S) and `cmake --build .` — both anchored by WORKING_DIRECTORY
# rather than -B. The outer then consumes the sub-build's artifacts.
file(MAKE_DIRECTORY ${CMAKE_BINARY_DIR}/subbuild)
execute_process(
  COMMAND ${CMAKE_COMMAND} -DSUB_VALUE=7 -G Ninja ${CMAKE_CURRENT_SOURCE_DIR}/sub
  WORKING_DIRECTORY ${CMAKE_BINARY_DIR}/subbuild
  RESULT_VARIABLE _sub_cfg)
execute_process(
  COMMAND ${CMAKE_COMMAND} --build .
  WORKING_DIRECTORY ${CMAKE_BINARY_DIR}/subbuild
  RESULT_VARIABLE _sub_bld)

add_executable(app main.c)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR}/subbuild)
target_link_libraries(app PRIVATE ${CMAKE_BINARY_DIR}/subbuild/libsublib.a)
