cmake_minimum_required(VERSION 3.20)
project(dlhttp C)

# Configure-time file(DOWNLOAD) of a header (file:// so it's offline +
# reproducible) with an integrity check. The bytes bake hermetically by
# default; the converter ALSO surfaces a `download` todo carrying the
# ready-to-paste http_file MODULE stanza (url + SRI integrity from
# EXPECTED_HASH) for an operator who wants the repo-rule form, and
# --lift-download emits the fetch genrule + download-repos.json lockfile.
file(DOWNLOAD
     file://${CMAKE_CURRENT_SOURCE_DIR}/payload.h.in
     ${CMAKE_CURRENT_BINARY_DIR}/dl_config.h
     EXPECTED_HASH SHA256=39cfaf3576b44a1166f9b96932a892ca63ddc22dd19adb1343ea1f5cbafb7361)

add_library(dlhttp STATIC src/lib.c)
target_include_directories(dlhttp PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
