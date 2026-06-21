cmake_minimum_required(VERSION 3.20)
project(shared_soversion C)

# A versioned SHARED library: cmake embeds a SONAME (libgreet.so.1, from
# SOVERSION) in the .so so a consumer's DT_NEEDED records the soversioned
# name, not the bare file. Faithful SHARED conversion must thread that soname
# onto the cc_shared_library (user_link_flags -Wl,-soname,...) — without it the
# bazel .so has no soname and diverges from cmake (the zlib ELF-lens gap).
add_library(greet SHARED greet.c)
set_target_properties(greet PROPERTIES VERSION 1.2.3 SOVERSION 1)

add_executable(app main.c)
target_link_libraries(app PRIVATE greet)
