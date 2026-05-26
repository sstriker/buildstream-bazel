# Hook injected via -DCMAKE_PROJECT_TOP_LEVEL_INCLUDES (cmake
# 3.24+) — Phase 3 of the generator-parity uplift (ROADMAP.md).
#
# Speculative-emit probe: for every cmake target in the project,
# emits per-target file(GENERATE) declarations that resolve common
# genex shapes to disk at generation time. cmake's own genex
# evaluator does the work, so the Go-side
# internal/genexeval's UnsupportedError surface (TARGET_OBJECTS,
# INTERFACE_* aggregation, cross-package TARGET_FILE, etc.) can
# retire — convert-element-cmake reads the resolved bytes via
# cmakerun.ReadGenexProbe.
#
# Output layout: <CMAKE_BINARY_DIR>/cmake-to-bazel.genex/
#   <tgt>/type.txt                  — $<TARGET_PROPERTY:tgt,TYPE>
#   <tgt>/file.<config>.txt         — $<TARGET_FILE:tgt>          (artifact-producing targets only — see gate)
#   <tgt>/file_dir.<config>.txt     — $<TARGET_FILE_DIR:tgt>      (artifact-producing targets only)
#   <tgt>/file_name.<config>.txt    — $<TARGET_FILE_NAME:tgt>     (artifact-producing targets only)
#   <tgt>/objects.<config>.txt      — $<TARGET_OBJECTS:tgt>       (only OBJECT_LIBRARY)
#   <tgt>/interface_<P>.<config>.txt
#                                   — $<TARGET_PROPERTY:tgt,INTERFACE_<P>>
#                                     for P in INCLUDE_DIRECTORIES /
#                                     COMPILE_DEFINITIONS / COMPILE_OPTIONS /
#                                     LINK_LIBRARIES / LINK_OPTIONS
#   <tgt>/property_<P>.<config>.txt — $<TARGET_PROPERTY:tgt,<P>> for non-INTERFACE_*
#
# The `.<config>.` infix is cmake's `$<CONFIG>` resolved at
# generation time: the build type ("Release") in single-config
# generators, the configuration name (one of CMAKE_CONFIGURATION_TYPES)
# in Ninja Multi-Config. Per-config OUTPUT paths are what makes the
# hook compose with multi-config; without it cmake errors with
# "Evaluation file to be written multiple times with different
# content" because the same OUTPUT would be generated once per
# config with potentially different CONTENT. type.txt stays
# config-invariant — TARGET_PROPERTY:TYPE doesn't honor
# `$<CONFIG>` so a single emit is correct.
#
# DEFER target: cmake_language(DEFER DIRECTORY) schedules the emit
# pass for end-of-top-level-directory processing, after every
# add_executable / add_library / target_link_libraries call in the
# user's CMakeLists.txt has run.
#
# The file(GENERATE) calls themselves don't fire until cmake's
# generation phase (post-configure), so the resolved bytes — even
# for target-property genexes that depend on transitive walks —
# are captured the same way the Ninja generator captures them.
#
# Why DEFER + speculative emit vs targeted emit driven by the
# trace: emitting all probes up-front in one configure pass means
# convert-element-cmake doesn't need a second cmake run after
# trace parsing, and the per-probe cost is small (cmake walks the
# generator-phase genex tree once per file(GENERATE) regardless).
# Output volume on FDSDK is ~1000 targets × ~6 properties = ~6000
# small files — well within filesystem limits.

if(NOT _CMTB_GENEX_PROBE_REGISTERED)
    set(_CMTB_GENEX_PROBE_REGISTERED TRUE)
    cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL _cmtb_probe_genex)
endif()

# CMP0112 NEW: reading $<TARGET_FILE:...> does NOT add an implicit
# target-dependency edge. The probe only wants to observe the
# resolved path, never to widen the build graph, so NEW is the
# correct stance. Without this, cmake spams one
#   "Policy CMP0112 is not set: Target file component generator
#    expressions do not add target dependencies."
# dev-warning per emitted file(GENERATE) — one per target × per
# variant — which can run into the hundreds on real-world projects.
cmake_policy(SET CMP0112 NEW)

# Recursively collect every BUILDSYSTEM_TARGETS entry from
# CMAKE_SOURCE_DIR's directory tree. Uses a global property as the
# accumulator because cmake function variable scopes don't compose
# cleanly across recursive calls without explicit PARENT_SCOPE
# threading per recursion level.
function(_cmtb_collect_targets _CMTB_DIR)
    get_property(_CMTB_LOCAL DIRECTORY "${_CMTB_DIR}" PROPERTY BUILDSYSTEM_TARGETS)
    foreach(_CMTB_TGT ${_CMTB_LOCAL})
        set_property(GLOBAL APPEND PROPERTY _CMTB_ALL_TARGETS "${_CMTB_TGT}")
    endforeach()
    get_property(_CMTB_SUBS DIRECTORY "${_CMTB_DIR}" PROPERTY SUBDIRECTORIES)
    foreach(_CMTB_SUB ${_CMTB_SUBS})
        _cmtb_collect_targets("${_CMTB_SUB}")
    endforeach()
endfunction()

function(_cmtb_probe_genex)
    set_property(GLOBAL PROPERTY _CMTB_ALL_TARGETS "")
    _cmtb_collect_targets("${CMAKE_SOURCE_DIR}")
    get_property(_CMTB_TGTS GLOBAL PROPERTY _CMTB_ALL_TARGETS)

    foreach(_CMTB_TGT ${_CMTB_TGTS})
        get_target_property(_CMTB_TYPE ${_CMTB_TGT} TYPE)
        set(_CMTB_OUT_DIR "${CMAKE_BINARY_DIR}/cmake-to-bazel.genex/${_CMTB_TGT}")

        # TYPE is always available — even on INTERFACE_LIBRARY and
        # ALIAS targets — so probe it unconditionally as the
        # consumer's gating signal.
        file(GENERATE
            OUTPUT "${_CMTB_OUT_DIR}/type.txt"
            CONTENT "$<TARGET_PROPERTY:${_CMTB_TGT},TYPE>")

        # $<TARGET_FILE:t> and its FILE_DIR / FILE_NAME variants
        # only resolve for targets that produce a single on-disk
        # artifact (EXECUTABLE / SHARED_LIBRARY / STATIC_LIBRARY /
        # MODULE_LIBRARY). INTERFACE_LIBRARY has no artifact;
        # UTILITY (add_custom_target) and ALIAS also don't produce
        # one. OBJECT_LIBRARY is also rejected: its "artifact" is
        # the list of .o files cmake emits per source, not a
        # linker-produced single file, so cmake fatal-errors with
        # "Target … is not an executable or library" when
        # $<TARGET_FILE:t> is asked for an OBJECT_LIBRARY (the .o
        # list lives under $<TARGET_OBJECTS:t> instead — see the
        # OBJECT_LIBRARY-specific branch below). Gate on the
        # affirmative set instead of an exclusion list so unknown
        # future types default to safe.
        if(_CMTB_TYPE STREQUAL "EXECUTABLE" OR
           _CMTB_TYPE STREQUAL "SHARED_LIBRARY" OR
           _CMTB_TYPE STREQUAL "STATIC_LIBRARY" OR
           _CMTB_TYPE STREQUAL "MODULE_LIBRARY")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/file.$<CONFIG>.txt"
                CONTENT "$<TARGET_FILE:${_CMTB_TGT}>")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/file_name.$<CONFIG>.txt"
                CONTENT "$<TARGET_FILE_NAME:${_CMTB_TGT}>")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/file_dir.$<CONFIG>.txt"
                CONTENT "$<TARGET_FILE_DIR:${_CMTB_TGT}>")
        endif()

        # $<TARGET_OBJECTS:t> resolves to a semicolon-separated list
        # of .o paths — meaningful only for OBJECT_LIBRARY targets
        # (other types either have no objects or roll them into the
        # archive / link).
        if(_CMTB_TYPE STREQUAL "OBJECT_LIBRARY")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/objects.$<CONFIG>.txt"
                CONTENT "$<TARGET_OBJECTS:${_CMTB_TGT}>")
        endif()

        # INTERFACE_* aggregates — cmake walks the dependency graph
        # at generation time and emits the post-walk values. This is
        # what internal/genexeval would otherwise need to reimplement
        # for INTERFACE_LINK_LIBRARIES / INTERFACE_INCLUDE_DIRECTORIES
        # and friends. Per-config OUTPUT because INTERFACE_*
        # properties can transitively pull in `$<CONFIG>`-bearing
        # generator expressions; emitting once per config is the
        # only safe shape under multi-config.
        foreach(_CMTB_PROP INCLUDE_DIRECTORIES COMPILE_DEFINITIONS COMPILE_OPTIONS LINK_LIBRARIES LINK_OPTIONS)
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/interface_${_CMTB_PROP}.$<CONFIG>.txt"
                CONTENT "$<TARGET_PROPERTY:${_CMTB_TGT},INTERFACE_${_CMTB_PROP}>")
        endforeach()

        # Non-INTERFACE per-target properties Bazel cc rules can
        # honor (rpath embeds, position-independent code,
        # visibility presets). The reader exposes these under
        # GenexProbe.Properties so consumers can route each into
        # the matching Bazel attribute (linkopts for rpath,
        # features = ["pic"] for PIC, etc.). Same per-config OUTPUT
        # rationale as the INTERFACE_* loop above.
        foreach(_CMTB_PROP BUILD_RPATH INSTALL_RPATH POSITION_INDEPENDENT_CODE CXX_VISIBILITY_PRESET C_VISIBILITY_PRESET VISIBILITY_INLINES_HIDDEN ENABLE_EXPORTS SOVERSION VERSION AUTOMOC AUTOUIC AUTORCC EXCLUDE_FROM_ALL MSVC_RUNTIME_LIBRARY JOB_POOL_COMPILE JOB_POOL_LINK CXX_EXTENSIONS C_EXTENSIONS)
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/property_${_CMTB_PROP}.$<CONFIG>.txt"
                CONTENT "$<TARGET_PROPERTY:${_CMTB_TGT},${_CMTB_PROP}>")
        endforeach()
    endforeach()
endfunction()
