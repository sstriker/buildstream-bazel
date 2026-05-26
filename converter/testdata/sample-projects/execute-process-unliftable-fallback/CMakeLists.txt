cmake_minimum_required(VERSION 3.23)
project(storage_cost C)

# Unliftable execute_process: the classifier sends git rev-parse
# + OUTPUT_VARIABLE to BucketStamp (a "stamp" — version string
# baked at configure time). With
# --unsupported-execute-process-fallback=true, the converter
# routes the whole element to the install-plan-driven placeholder
# shape: an extract genrule that untars install_tree.tar plus
# per-target cc_import / sh_binary stubs that reference the
# extracted files. Without the flag, the converter exits Tier-1
# with `unsupported-execute-process`.
#
# Why a stamp-shaped call: matches a real-world pattern (cmake
# projects that bake a build-time version stamp) and is the
# canonical example called out in
# docs/design/rendezvous.md.
# `git rev-parse HEAD` can't be safely re-executed at Bazel
# action time (the action would need read access to the .git
# directory; the version stamp would invalidate on every commit;
# the output is non-deterministic across a worktree refresh) so
# the lifter classifies it BucketStamp and refuses.
#
# The fixture committed here uses `cmake -E echo` so
# tools/fixtures/record-fileapi.sh can configure the project
# even outside a git checkout (cmake's File API captures the
# call shape regardless of which command was executed). The
# *recorded* classifier signal — non-cmake driver + OUTPUT_VARIABLE
# — is what routes to BucketStamp. Operators reading this can
# swap the COMMAND for `git rev-parse HEAD` in a real project;
# the classifier dispatches identically.
execute_process(
    COMMAND ${CMAKE_COMMAND} -E echo "stub-sha-for-record-fileapi"
    OUTPUT_VARIABLE BUILD_VERSION
    OUTPUT_STRIP_TRAILING_WHITESPACE
)

add_library(foo STATIC src/foo.c)
add_library(bar SHARED src/bar.c)
add_executable(baz src/baz.c)

# FILE_SET HEADERS — cmake's modern (3.23+) declarative header
# tracking. emitFallbackPlaceholder picks these up and threads
# them into both the per-target stub's hdrs AND the extract
# genrule's outs. install(FILES ...) below covers the
# legacy-style bag-of-files install — those headers land in
# install_tree.tar but no cc_import references them at extract
# time, so they're the "stays-in-tar-only" leg of the storage
# duplication.
target_sources(foo
    PUBLIC FILE_SET HEADERS BASE_DIRS include FILES
        include/foo.h
)
target_sources(bar
    PUBLIC FILE_SET HEADERS BASE_DIRS include FILES
        include/bar.h
)

target_include_directories(baz PRIVATE include)

install(TARGETS foo
    ARCHIVE DESTINATION lib
    FILE_SET HEADERS DESTINATION include
)
install(TARGETS bar
    LIBRARY DESTINATION lib
    FILE_SET HEADERS DESTINATION include
)
install(TARGETS baz
    RUNTIME DESTINATION bin
)
# Legacy install(FILES ...) — a header that the FILE_SET path
# doesn't capture. Stays in install_tree.tar; no extract genrule
# entry. Realistic shape: many cmake projects mix FILE_SET HEADERS
# for new code with install(FILES ...) for legacy/external bits.
install(FILES include/baz.h
    DESTINATION include
)
