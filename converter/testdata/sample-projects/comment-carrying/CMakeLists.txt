# Copyright 2026 the comment-carrying authors.
# SPDX-License-Identifier: Apache-2.0

cmake_minimum_required(VERSION 3.20)
project(commentcarrying C)

# The core library — wraps the vendored widget code.
# Built static for the embedded target.
add_library(widget STATIC src/widget.c)  # the widget core lib

# Generate the lookup table from the spec.
add_custom_command(
  OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/table.h
  COMMAND ${CMAKE_COMMAND} -E echo "// generated" > ${CMAKE_CURRENT_BINARY_DIR}/table.h
  VERBATIM)
add_custom_target(gen_table DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/table.h)
