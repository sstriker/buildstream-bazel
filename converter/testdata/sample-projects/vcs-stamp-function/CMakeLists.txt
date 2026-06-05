cmake_minimum_required(VERSION 3.20)
project(vcsstampfunction C)

# VCS stamp via a HELPER FUNCTION (the GetGitRevisionDescription.cmake /
# git_describe() shape, as used by SDL): the execute_process OUTPUT_VARIABLE
# is a function-LOCAL the dump-vars top-level snapshot can't see, and the
# value is handed back via `set(${_var} "${out}" PARENT_SCOPE)`. A single
# clean (non-diagnostic) convert refuses the stamp (the local isn't captured)
# and aborts in pass 1; the converter recovers the set()-copy via the warm
# non-expanded-trace configure, and the forwarded-stamp rescue (OUTPUT_VARIABLE
# is the SrcVar of that copy) lifts the call so the convert completes.
function(get_git_sha _var)
  execute_process(
    COMMAND git rev-parse HEAD
    WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
    OUTPUT_VARIABLE out
    OUTPUT_STRIP_TRAILING_WHITESPACE)
  set(${_var} "${out}" PARENT_SCOPE)
endfunction()

get_git_sha(GIT_SHA)

configure_file(
  ${CMAKE_CURRENT_SOURCE_DIR}/src/version.h.in
  ${CMAKE_CURRENT_BINARY_DIR}/version.h
  @ONLY)

add_library(vcsstampfunction STATIC src/version.c)
target_include_directories(vcsstampfunction PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
