cmake_minimum_required(VERSION 3.20)
project(probe_rescue LANGUAGES C)

find_program(PY3 NAMES python3 REQUIRED)

# A top-level execute_process PROBE: python3 is a dual-use probe
# driver, so a `python3 -c ...` call that captures stdout into
# OUTPUT_VARIABLE (no OUTPUT_FILE) classifies as BucketProbe in
# the converter's Classify(). cmake runs it at configure time and
# the dump-vars hook captures PROBE_RESULT at end-of-configure.
execute_process(
    COMMAND ${PY3} -c "print('probe-value-42')"
    OUTPUT_VARIABLE PROBE_RESULT
    OUTPUT_STRIP_TRAILING_WHITESPACE)

# A configure_file consuming the probe value via @PROBE_RESULT@.
# With --lift-configure-file the recovered genrule re-renders this
# template at Bazel time from the captured cmake variable namespace
# (which includes PROBE_RESULT via the dump-vars rescue).
configure_file(
    ${CMAKE_CURRENT_SOURCE_DIR}/src/config.h.in
    ${CMAKE_CURRENT_BINARY_DIR}/config.h
    @ONLY)

add_library(probelib STATIC src/probelib.c)
target_include_directories(probelib PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/src>
    $<BUILD_INTERFACE:${CMAKE_CURRENT_BINARY_DIR}>
    $<INSTALL_INTERFACE:include>)

install(TARGETS probelib ARCHIVE DESTINATION lib)
