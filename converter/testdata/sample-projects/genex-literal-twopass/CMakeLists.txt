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

# CONTENT-body tier (b′) case: two ADJACENT genexes with no static
# separator between them. The Go-side (a) evaluator refuses arbitrary
# TARGET_PROPERTY, and the (b) structured-capture extractor refuses
# adjacent genexes (it can't tell where the first value ends and the
# second begins by positional anchoring). The per-literal two-pass
# probe resolves each genex individually via cmake's own evaluator
# ("alpha", "beta") and replays them as a literal-replace map — so the
# body lifts (cmake-codegen-genex-resolved) instead of baking the
# rendered bytes (cmake-codegen-genex-unresolved). The OUTPUT here is
# static, so unlike the manifest.txt case the genrule is emitted
# either way; only the resolved-vs-baked tag flips with the second pass.
set_property(TARGET app PROPERTY GEN_PART_A "alpha")
set_property(TARGET app PROPERTY GEN_PART_B "beta")
file(GENERATE
     OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/content_adjacent.txt"
     CONTENT "$<TARGET_PROPERTY:app,GEN_PART_A>$<TARGET_PROPERTY:app,GEN_PART_B>\n")
