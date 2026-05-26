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
#   <tgt>/type.txt          — $<TARGET_PROPERTY:tgt,TYPE>
#   <tgt>/file.txt          — $<TARGET_FILE:tgt>          (skipped for INTERFACE_LIBRARY)
#   <tgt>/file_dir.txt      — $<TARGET_FILE_DIR:tgt>      (skipped for INTERFACE_LIBRARY)
#   <tgt>/file_name.txt     — $<TARGET_FILE_NAME:tgt>     (skipped for INTERFACE_LIBRARY)
#   <tgt>/objects.txt       — $<TARGET_OBJECTS:tgt>       (only OBJECT_LIBRARY)
#   <tgt>/interface_<P>.txt — $<TARGET_PROPERTY:tgt,INTERFACE_<P>>
#                              for P in INCLUDE_DIRECTORIES /
#                              COMPILE_DEFINITIONS / COMPILE_OPTIONS /
#                              LINK_LIBRARIES / LINK_OPTIONS
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

        # $<TARGET_FILE:t> and its FILE_DIR / FILE_NAME variants are
        # undefined on INTERFACE_LIBRARY targets (no on-disk
        # artifact). Skip cleanly for those.
        if(NOT _CMTB_TYPE STREQUAL "INTERFACE_LIBRARY")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/file.txt"
                CONTENT "$<TARGET_FILE:${_CMTB_TGT}>")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/file_name.txt"
                CONTENT "$<TARGET_FILE_NAME:${_CMTB_TGT}>")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/file_dir.txt"
                CONTENT "$<TARGET_FILE_DIR:${_CMTB_TGT}>")
        endif()

        # $<TARGET_OBJECTS:t> resolves to a semicolon-separated list
        # of .o paths — meaningful only for OBJECT_LIBRARY targets
        # (other types either have no objects or roll them into the
        # archive / link).
        if(_CMTB_TYPE STREQUAL "OBJECT_LIBRARY")
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/objects.txt"
                CONTENT "$<TARGET_OBJECTS:${_CMTB_TGT}>")
        endif()

        # INTERFACE_* aggregates — cmake walks the dependency graph
        # at generation time and emits the post-walk values. This is
        # what internal/genexeval would otherwise need to reimplement
        # for INTERFACE_LINK_LIBRARIES / INTERFACE_INCLUDE_DIRECTORIES
        # and friends.
        foreach(_CMTB_PROP INCLUDE_DIRECTORIES COMPILE_DEFINITIONS COMPILE_OPTIONS LINK_LIBRARIES LINK_OPTIONS)
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/interface_${_CMTB_PROP}.txt"
                CONTENT "$<TARGET_PROPERTY:${_CMTB_TGT},INTERFACE_${_CMTB_PROP}>")
        endforeach()

        # Non-INTERFACE per-target properties Bazel cc rules can
        # honor (rpath embeds, position-independent code,
        # visibility presets). The reader exposes these under
        # GenexProbe.Properties so consumers can route each into
        # the matching Bazel attribute (linkopts for rpath,
        # features = ["pic"] for PIC, etc.).
        foreach(_CMTB_PROP BUILD_RPATH INSTALL_RPATH POSITION_INDEPENDENT_CODE CXX_VISIBILITY_PRESET C_VISIBILITY_PRESET VISIBILITY_INLINES_HIDDEN ENABLE_EXPORTS SOVERSION VERSION AUTOMOC AUTOUIC AUTORCC EXCLUDE_FROM_ALL MSVC_RUNTIME_LIBRARY JOB_POOL_COMPILE JOB_POOL_LINK CXX_EXTENSIONS C_EXTENSIONS)
            file(GENERATE
                OUTPUT "${_CMTB_OUT_DIR}/property_${_CMTB_PROP}.txt"
                CONTENT "$<TARGET_PROPERTY:${_CMTB_TGT},${_CMTB_PROP}>")
        endforeach()
    endforeach()
endfunction()
