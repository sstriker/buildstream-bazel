# Copyright 2026 the comment-carrying authors.
# SPDX-License-Identifier: Apache-2.0

cmake_minimum_required(VERSION 3.20)
project(commentcarrying C)

# The core library — wraps the vendored widget code.
# Built static for the embedded target.
add_library(widget STATIC src/widget.c)  # the widget core lib

# Helper macro — its body comment must NOT land on the targets it makes.
macro(add_gadget_lib name)
  # inside the macro body
  add_library(${name} STATIC src/gadget.c)
endmacro()

# The gadget lib — declared via the helper macro.
add_gadget_lib(gadget)  # macro-made gadget

# Two more libs declared directly inside an included file — each must keep
# the comment above its OWN add_library, not lose it to the include() site.
include(cmake/extra.cmake)

# A header-only lib via a helper FUNCTION in an included module: the trace
# frame stack must carry the invocation's comment (codemodel drops INTERFACE
# libs, so this rides the trace-synth lift).
include(cmake/iface_helpers.cmake)

# The gizmo interface lib — declared via the helper function.
add_iface_lib(gizmo)  # trace-synth gizmo

# Generate the lookup table from the spec.
add_custom_command(
  OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/table.h
  COMMAND ${CMAKE_COMMAND} -E echo "// generated" > ${CMAKE_CURRENT_BINARY_DIR}/table.h
  VERBATIM)
add_custom_target(gen_table DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/table.h)
