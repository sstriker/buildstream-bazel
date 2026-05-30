cmake_minimum_required(VERSION 3.24)
project(genex_literal_twopass LANGUAGES C)

add_library(app STATIC src/app.c)

# A custom target property holding the generated-file subdirectory.
# This is NOT in the structural probe's fixed set (TARGET_FILE /
# TARGET_OBJECTS / INTERFACE_*), and the Go-side evaluator does not
# model arbitrary TARGET_PROPERTY reads, so pass 1 cannot resolve
# $<TARGET_PROPERTY:app,APP_GENDIR> — it records a literal-probe
# request. The warm second configure pass resolves it via cmake's
# own evaluator to the literal "gen_out".
set_property(TARGET app PROPERTY APP_GENDIR "gen_out")

# file(GENERATE) whose OUTPUT path carries the unresolved genex. The
# OUTPUT must resolve at convert time (it becomes the genrule's
# outs); the second configure pass — still pre-Bazel — supplies it.
file(GENERATE
     OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/$<TARGET_PROPERTY:app,APP_GENDIR>/manifest.txt"
     CONTENT "generated-manifest-line\n")
