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

# Macro declaring TWO libs from one invocation — the invocation's comment
# must carry to BOTH (the uniform multi-target-per-invocation policy).
macro(add_lib_pair name)
  add_library(${name}_a STATIC src/pair_a.c)
  add_library(${name}_b STATIC src/pair_b.c)
endmacro()

# Both halves of the pair — one comment, two rules.
add_lib_pair(thing)

# Macro wrapping a codegen pair (command + driver target) — the comment on
# the invocation below must carry to the synthesized genrule.
macro(gen_lut name)
  # inside the codegen macro
  add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/${name}.h
    COMMAND ${CMAKE_COMMAND} -E echo "// generated lut" > ${CMAKE_CURRENT_BINARY_DIR}/${name}.h
    VERBATIM)
  add_custom_target(gen_${name} DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/${name}.h)
endmacro()

# Generate the LUT — wrapped in the codegen macro.
gen_lut(lut)  # macro-made lut

# Generate the lookup table from the spec.
add_custom_command(
  OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/table.h
  COMMAND ${CMAKE_COMMAND} -E echo "// generated" > ${CMAKE_CURRENT_BINARY_DIR}/table.h
  VERBATIM)
add_custom_target(gen_table DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/table.h)
