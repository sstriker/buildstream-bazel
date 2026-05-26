# Example: per-config sanitizer flag definitions on the cmake side.
#
# This is what an operator's CMakeLists.txt looks like when they
# follow the cmake idiom for sanitizer variants. The converter
# picks up CMAKE_CONFIGURATION_TYPES + CMAKE_<LANG>_FLAGS_<NAME>
# via the codemodel and routes through configfold; the actual
# flag values DO NOT flow into the emitted BUILD.bazel — they
# live in the operator's cc_toolchain feature definitions
# (see ../toolchain/features.bzl) instead.
#
# Operators converting an existing cmake project don't need to
# add these blocks specifically for Bazel — if the project
# already builds under cmake with --config ASan, the per-config
# flag definitions are already in place. The converter
# recognizes the config names automatically via
# configfold.SanitizerFeature.
#
# Note on the _INIT pattern: cmake's standard initialization
# pre-populates the cache with empty entries for every
# CMAKE_<LANG>_FLAGS_<CONFIG> in CMAKE_CONFIGURATION_TYPES
# BEFORE any set() in CMakeLists.txt runs. A plain
# `set(CMAKE_C_FLAGS_ASAN "..." CACHE STRING "...")` after
# project() is a no-op on first configure because the empty
# cache entry already exists. The fix is to seed
# CMAKE_<LANG>_FLAGS_<CONFIG>_INIT before project() runs —
# cmake's standard cache-init path consults the _INIT vars
# when creating the cache entries. The `if(NOT DEFINED ...)`
# guards let operators override on the cmake command line
# (e.g. -DCMAKE_C_FLAGS_ASAN_INIT="...").

cmake_minimum_required(VERSION 3.20)

# Address Sanitizer.
if(NOT DEFINED CMAKE_C_FLAGS_ASAN_INIT)
    set(CMAKE_C_FLAGS_ASAN_INIT
        "-fsanitize=address -fno-omit-frame-pointer -g -O1")
endif()
if(NOT DEFINED CMAKE_CXX_FLAGS_ASAN_INIT)
    set(CMAKE_CXX_FLAGS_ASAN_INIT
        "-fsanitize=address -fno-omit-frame-pointer -g -O1")
endif()
if(NOT DEFINED CMAKE_EXE_LINKER_FLAGS_ASAN_INIT)
    set(CMAKE_EXE_LINKER_FLAGS_ASAN_INIT
        "-fsanitize=address")
endif()
if(NOT DEFINED CMAKE_SHARED_LINKER_FLAGS_ASAN_INIT)
    set(CMAKE_SHARED_LINKER_FLAGS_ASAN_INIT
        "-fsanitize=address")
endif()

# Thread Sanitizer.
if(NOT DEFINED CMAKE_C_FLAGS_TSAN_INIT)
    set(CMAKE_C_FLAGS_TSAN_INIT
        "-fsanitize=thread -g -O1")
endif()
if(NOT DEFINED CMAKE_CXX_FLAGS_TSAN_INIT)
    set(CMAKE_CXX_FLAGS_TSAN_INIT
        "-fsanitize=thread -g -O1")
endif()
if(NOT DEFINED CMAKE_EXE_LINKER_FLAGS_TSAN_INIT)
    set(CMAKE_EXE_LINKER_FLAGS_TSAN_INIT
        "-fsanitize=thread")
endif()
if(NOT DEFINED CMAKE_SHARED_LINKER_FLAGS_TSAN_INIT)
    set(CMAKE_SHARED_LINKER_FLAGS_TSAN_INIT
        "-fsanitize=thread")
endif()

# Undefined Behavior Sanitizer.
if(NOT DEFINED CMAKE_C_FLAGS_UBSAN_INIT)
    set(CMAKE_C_FLAGS_UBSAN_INIT
        "-fsanitize=undefined -fno-omit-frame-pointer -g -O1")
endif()
if(NOT DEFINED CMAKE_CXX_FLAGS_UBSAN_INIT)
    set(CMAKE_CXX_FLAGS_UBSAN_INIT
        "-fsanitize=undefined -fno-omit-frame-pointer -g -O1")
endif()
if(NOT DEFINED CMAKE_EXE_LINKER_FLAGS_UBSAN_INIT)
    set(CMAKE_EXE_LINKER_FLAGS_UBSAN_INIT
        "-fsanitize=undefined")
endif()
if(NOT DEFINED CMAKE_SHARED_LINKER_FLAGS_UBSAN_INIT)
    set(CMAKE_SHARED_LINKER_FLAGS_UBSAN_INIT
        "-fsanitize=undefined")
endif()

# Coverage.
if(NOT DEFINED CMAKE_C_FLAGS_COVERAGE_INIT)
    set(CMAKE_C_FLAGS_COVERAGE_INIT
        "--coverage -g -O0")
endif()
if(NOT DEFINED CMAKE_CXX_FLAGS_COVERAGE_INIT)
    set(CMAKE_CXX_FLAGS_COVERAGE_INIT
        "--coverage -g -O0")
endif()
if(NOT DEFINED CMAKE_EXE_LINKER_FLAGS_COVERAGE_INIT)
    set(CMAKE_EXE_LINKER_FLAGS_COVERAGE_INIT
        "--coverage")
endif()

project(sanitizer_example C CXX)

# Mark the per-config flag cache vars advanced so cmake-gui
# doesn't clutter its top-level view with them. The cache
# entries are created by cmake's standard initialization from
# the _INIT vars above; this just hides them from the default
# gui view.
mark_as_advanced(
    CMAKE_C_FLAGS_ASAN CMAKE_CXX_FLAGS_ASAN
    CMAKE_EXE_LINKER_FLAGS_ASAN CMAKE_SHARED_LINKER_FLAGS_ASAN
    CMAKE_C_FLAGS_TSAN CMAKE_CXX_FLAGS_TSAN
    CMAKE_EXE_LINKER_FLAGS_TSAN CMAKE_SHARED_LINKER_FLAGS_TSAN
    CMAKE_C_FLAGS_UBSAN CMAKE_CXX_FLAGS_UBSAN
    CMAKE_EXE_LINKER_FLAGS_UBSAN CMAKE_SHARED_LINKER_FLAGS_UBSAN
    CMAKE_C_FLAGS_COVERAGE CMAKE_CXX_FLAGS_COVERAGE
    CMAKE_EXE_LINKER_FLAGS_COVERAGE
)

# Sample targets — these compile under any of the configurations
# above without source-side opt-in. The sanitizer flag set picks
# up automatically from CMAKE_BUILD_TYPE / the multi-config arm.
add_library(mylib STATIC src/lib.c)
target_include_directories(mylib PUBLIC include)

add_executable(myapp src/main.c)
target_link_libraries(myapp PRIVATE mylib)
