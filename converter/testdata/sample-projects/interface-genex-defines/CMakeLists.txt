cmake_minimum_required(VERSION 3.24)
project(interface_genex_defines LANGUAGES C)

# An INTERFACE library whose INTERFACE_COMPILE_DEFINITIONS mix a
# plain define with config-gated genex. cmake's File API omits
# INTERFACE_LIBRARY targets from its codemodel targets[], so the
# converter synthesizes this cc_library from the trace. The Go-side
# evaluator can't crack the nested $<$<CONFIG:...>:...> shape, so
# without the structural-probe reconciliation the config-gated
# define is silently DROPPED. With --probe-genex, cmake's own
# resolved INTERFACE_COMPILE_DEFINITIONS (for the configured build
# type) is captured and used, so the define survives.
add_library(iface INTERFACE)
target_compile_definitions(iface INTERFACE
  PLAIN_DEF=1
  $<$<CONFIG:Debug>:DEBUG_ONLY=1>
  $<$<CONFIG:Release>:RELEASE_ONLY=1>)

add_library(app STATIC src/app.c)
target_link_libraries(app iface)
