cmake_minimum_required(VERSION 3.20)
project(probe_genex_utility C)

# Real library — exercises probe-genex's affirmative-type gate
# (STATIC_LIBRARY hits the TARGET_FILE branch).
add_library(realtarget STATIC realtarget.c)

# UTILITY target — the round-1 bug. probe-genex's $<TARGET_FILE:>
# evaluation fatal-errors on UTILITY targets with "Target ... is
# not an executable or library". boost has tests / check / etc.;
# llvm has install-* utility targets; any project using
# add_custom_target trips this when --probe-genex is on.
#
# Round-1 fix (PR #227, commit 2f67f4b) flipped the gate from an
# exclusion list (NOT INTERFACE_LIBRARY) to an affirmative
# inclusion of artifact-producing types. UTILITY now falls outside
# the gate and TARGET_FILE is never emitted for it — cmake's
# generation step succeeds instead of aborting.
add_custom_target(noisy_utility
    COMMAND ${CMAKE_COMMAND} -E echo "noisy_utility ran"
)

# ALIAS — also rejected by TARGET_FILE; same gate covers it.
add_library(realtarget_alias ALIAS realtarget)

# INTERFACE_LIBRARY — was already excluded by the original gate;
# included here as a regression check that the new gate still
# excludes it.
add_library(headers_only INTERFACE)
target_include_directories(headers_only INTERFACE ${CMAKE_CURRENT_SOURCE_DIR})
