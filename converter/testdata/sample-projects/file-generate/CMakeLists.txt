cmake_minimum_required(VERSION 3.20)
project(fglib LANGUAGES C VERSION 0.9.0)

# Exercises three file(GENERATE) shapes the lifter handles:
#
# 1. INPUT form, genex-free: template lives in source tree;
#    file(GENERATE) emits the body verbatim (no @VAR@/${VAR}
#    substitution — those are configure_file's surface, not
#    file(GENERATE)'s). Lifter should recognize template ==
#    rendered (via the verify-pass) and emit a genrule with
#    srcs=<template> + tools=//tools:cmake-configure-file
#    invoked with an empty values dict.
file(GENERATE
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/version.h
    INPUT  ${CMAKE_CURRENT_SOURCE_DIR}/src/version.h.in)

# 2. CONTENT form, genex-free: the body is inline. cmake's
#    argument parsing has already expanded ${PROJECT_VERSION}
#    by the time the trace records the call, so the recorded
#    Content is the literal post-expansion string. Lifter emits
#    a genrule using cmake-configure-file's --content-base64
#    mode.
file(GENERATE
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/banner.h
    CONTENT "#define BANNER \"fglib ${PROJECT_VERSION}\"\n")

# 3. CONTENT form with a generator expression: $<CONFIG>
#    resolves at generate-time. Lifter's hasGenex short-circuit
#    routes this to the legacy bytes-embedded shape and tags it
#    cmake-codegen-genex-unresolved so the audit can find
#    the lift refusal.
file(GENERATE
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/config_tag.h
    CONTENT "#define BUILD_TAG \"$<CONFIG>\"\n")

add_library(fglib STATIC src/fglib.c)
target_include_directories(fglib PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/src>
    $<BUILD_INTERFACE:${CMAKE_CURRENT_BINARY_DIR}>
    $<INSTALL_INTERFACE:include>)

install(TARGETS fglib
        EXPORT fglibTargets
        ARCHIVE DESTINATION lib)
install(EXPORT fglibTargets
        FILE fglibTargets.cmake
        NAMESPACE fglib::
        DESTINATION lib/cmake/fglib)
