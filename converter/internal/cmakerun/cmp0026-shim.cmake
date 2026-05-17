# Compatibility shim for cmake 4.x removal of CMP0026 OLD behaviour.
# Injected via -DCMAKE_PROJECT_TOP_LEVEL_INCLUDES (cmake 3.24+) so
# the override is installed at the end of every project() call,
# BEFORE any user code that calls get_target_property runs.
#
# What it does
# ------------
# Wraps get_target_property so that LOCATION / LOCATION_<CONFIG>
# queries return the cmake-team-recommended migration target —
# $<TARGET_FILE:<tgt>> — instead of fatal-erring with the cmake 4.x
# diagnostic
#
#   The LOCATION property may not be read from target "<tgt>".
#   ... CMP0026 ... policy CMP0026 was removed
#
# All other property reads pass through to the built-in
# implementation unchanged via cmake's `_<builtin>` recovery name
# (cmake exposes the original built-in command under the
# underscore-prefixed alias whenever a `function()` of the same
# name installs a wrapper).
#
# Caveats
# -------
# The generator-expression return shape is only equivalent to
# LOCATION when the value is consumed by something that evaluates
# generator expressions at build-system-generation time
# (add_custom_command, install(), file(GENERATE), TARGET_<>
# properties, etc.). Code that string-composes the LOCATION value
# at configure time (e.g. `message(STATUS "${LOC}")` or
# `set(SOMETHING "${LOC}.bak")`) will see literal
# `$<TARGET_FILE:foo>` text — that's the same surface as
# rewriting the call site to TARGET_FILE manually, which is the
# cmake-team-recommended migration.
#
# The shim is opt-in (`convert-element-cmake --cmp0026-shim`) so
# operators who can stomach the trade-off enable it; the default
# stays "surface the fatal error with the [hint] block pointing at
# the patch_cmds workaround". See #208.

# Self-versioned re-include guard. CMAKE_PROJECT_TOP_LEVEL_INCLUDES
# fires once per top-level project() call; a fixture with multiple
# project() statements would otherwise install the wrapper twice,
# and the second pass would recursively call its own outer wrapper
# (the `_get_target_property` rename only saves one level of
# original).
if(DEFINED _CMTB_CMP0026_SHIM_INSTALLED)
    return()
endif()
set(_CMTB_CMP0026_SHIM_INSTALLED TRUE)

function(get_target_property var tgt prop)
    if(prop MATCHES "^LOCATION(_.+)?$")
        set(${var} "$<TARGET_FILE:${tgt}>" PARENT_SCOPE)
        return()
    endif()
    _get_target_property(_cmtb_shim_value ${tgt} ${prop})
    set(${var} "${_cmtb_shim_value}" PARENT_SCOPE)
endfunction()
