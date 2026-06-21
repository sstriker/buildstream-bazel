cmake_minimum_required(VERSION 3.20)
project(shared_soversion C)

# A versioned SHARED library with a symbol version script — exercising BOTH
# halves of cc_shared_library ELF fidelity:
#   - SONAME: cmake embeds libgreet.so.1 (from SOVERSION) so a consumer's
#     DT_NEEDED records the soversioned name. The conversion must thread it onto
#     the cc_shared_library (user_link_flags -Wl,-soname,...).
#   - version script: greet.map exports only `greet` (GREET_1.0), hiding the
#     internal symbol, producing .gnu.version_d (VERDEF) nodes. The conversion
#     must route the -Wl,--version-script onto the WRAPPER's user_link_flags
#     (as $(location ...)) + stage greet.map via additional_linker_inputs — not
#     drop it (default static-collapse) and not put it on the impl cc_library
#     (whose linkopts propagate to consumers).
add_library(greet SHARED greet.c)
set_target_properties(greet PROPERTIES VERSION 1.2.3 SOVERSION 1)
target_link_options(greet PRIVATE "-Wl,--version-script,${CMAKE_CURRENT_SOURCE_DIR}/greet.map")

add_executable(app main.c)
target_link_libraries(app PRIVATE greet)
