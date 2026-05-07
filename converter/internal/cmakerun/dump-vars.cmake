# Hook injected via -DCMAKE_PROJECT_TOP_LEVEL_INCLUDES (cmake
# 3.24+; we tried -DCMAKE_PROJECT_INCLUDE_AFTER first but
# -D didn't reliably propagate that variable in our test fixture
# — see the run.go argv comment for the diagnostic). Registers a
# deferred callback that fires at the end of the top-level
# directory's processing, AFTER all configure_file() / set() /
# option() calls have run; dumps every cmake variable's value
# so the configurefile lift's Bazel-time substitution has access
# to the same namespace cmake had at configure time.
#
# Why DEFER (not just dump inline): TOP_LEVEL_INCLUDES files are
# included as part of the top-level project() call but BEFORE the
# user's subsequent commands (configure_file, set(), etc). If we
# dumped here we'd miss every variable the user sets later in
# CMakeLists.txt. DEFER schedules the dump for end-of-top-level-
# directory, which fires after every command in CMakeLists.txt
# has executed.
#
# Output format: one line per variable, "<NAME>=<HEX>\n", where
# HEX is the lowercase hex encoding of the value's bytes
# (string(HEX) is cmake 3.18+; the project's architectural floor
# is 3.20). Hex is robust against values that contain newlines,
# quotes, backslashes, semicolons, or anything else cmake might
# surface — Go's encoding/hex inverts it cleanly. The Go-side
# parser is converter/internal/cmakerun.parseVarsDump.

if(NOT _CMTB_VARS_DUMP_REGISTERED)
    set(_CMTB_VARS_DUMP_REGISTERED TRUE)
    cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL _cmtb_dump_vars)
endif()

function(_cmtb_dump_vars)
    # All function-locals are _CMTB_-prefixed so the
    # `^_CMTB_` filter below catches them — without that, if
    # `get_cmake_property(... VARIABLES)` enumerates the
    # current function's locals (cmake's docs are ambiguous on
    # whether function scope is included; observed behavior
    # varies by version), our internals would leak into the
    # dump. The most dangerous case is _CMTB_BUF, which grows
    # one entry per iteration; if it ever appeared in the
    # variable list mid-loop it would self-capture into the
    # output, ballooning the values JSON unboundedly.
    get_cmake_property(_CMTB_VARS VARIABLES)
    set(_CMTB_BUF "")
    foreach(_CMTB_VAR ${_CMTB_VARS})
        # Skip the registration sentinel + this function's own
        # locals so they don't leak into the output. Conservative
        # name prefix so we don't accidentally swallow a user
        # variable named _CMTB_*.
        if(_CMTB_VAR MATCHES "^_CMTB_")
            continue()
        endif()
        # ${${_CMTB_VAR}} is the value of the variable named
        # _CMTB_VAR. Cmake variables are themselves strings (with
        # embedded ; treated as list separators); ${...} re-joins
        # with ; which is the same shape the configure_file
        # substitutor consumes, so we capture it as-is.
        set(_CMTB_VALUE "${${_CMTB_VAR}}")
        string(HEX "${_CMTB_VALUE}" _CMTB_HEX)
        string(APPEND _CMTB_BUF "${_CMTB_VAR}=${_CMTB_HEX}\n")
    endforeach()
    file(WRITE "${CMAKE_BINARY_DIR}/cmake-to-bazel.vars.dump" "${_CMTB_BUF}")
endfunction()
