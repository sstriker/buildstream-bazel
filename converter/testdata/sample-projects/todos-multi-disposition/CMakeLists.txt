cmake_minimum_required(VERSION 3.24)
project(todos_multi_disposition LANGUAGES C)

# A multi-disposition fixture: one project that exercises ALL THREE
# conversion-todos dispositions end-to-end (actionable / improvement /
# informational), so the render gate proves the qualifier flows through
# real cmake — not just the Go unit tests. See
# scripts/meta-cmake-todos-coverage.sh.

# ---------------------------------------------------------------------------
# actionable — a cmake -P integration harness (the canonical brotli shape).
# There is no faithful mechanical translation of an `add_test(... cmake -P
# <runner>)`; an author must re-express it as an sh_test / diff_test over the
# built CLI. The converter emits a `cmake-p-test` todo tagged actionable.
# ---------------------------------------------------------------------------
add_executable(cli src/cli.c)

enable_testing()
add_test(NAME cli_roundtrip
         COMMAND ${CMAKE_COMMAND}
                 -DCLI=$<TARGET_FILE:cli>
                 -P ${CMAKE_CURRENT_SOURCE_DIR}/run_test.cmake hello)

# ---------------------------------------------------------------------------
# improvement — a file(GENERATE) whose CONTENT body reads an arbitrary
# TARGET_PROPERTY the Go-side evaluator does not model. The converter bakes
# the rendered bytes at convert time (a working but frozen result); an author
# could replace it with a dynamic Bazel form. Emitted as a `bake` todo tagged
# improvement. (Same shape as the genex-literal-twopass fixture.)
# ---------------------------------------------------------------------------
add_library(app STATIC src/app.c)
set_property(TARGET app PROPERTY GEN_TAG "frozen-value")
file(GENERATE
     OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/app_manifest.txt"
     CONTENT "tag=$<TARGET_PROPERTY:app,GEN_TAG>\n")

# ---------------------------------------------------------------------------
# informational — a `cmake -E create_symlink` edge. cmake's portable symlink
# primitive has no content-producing Bazel analogue (Bazel models tool aliases
# natively); the converter drops the edge with a breadcrumb. Emitted as a
# `cmake-internal-drop` todo tagged informational.
# ---------------------------------------------------------------------------
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/cli_alias
    COMMAND ${CMAKE_COMMAND} -E create_symlink
            $<TARGET_FILE:cli> ${CMAKE_CURRENT_BINARY_DIR}/cli_alias
    DEPENDS cli
    VERBATIM)
add_custom_target(make_alias ALL
    DEPENDS ${CMAKE_CURRENT_BINARY_DIR}/cli_alias)
