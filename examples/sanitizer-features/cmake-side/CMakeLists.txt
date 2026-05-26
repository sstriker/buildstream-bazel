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

cmake_minimum_required(VERSION 3.20)
project(sanitizer_example C CXX)

# Address Sanitizer.
set(CMAKE_C_FLAGS_ASAN
    "-fsanitize=address -fno-omit-frame-pointer -g -O1"
    CACHE STRING "Flags used by the C compiler during ASan builds.")
set(CMAKE_CXX_FLAGS_ASAN
    "-fsanitize=address -fno-omit-frame-pointer -g -O1"
    CACHE STRING "Flags used by the C++ compiler during ASan builds.")
set(CMAKE_EXE_LINKER_FLAGS_ASAN
    "-fsanitize=address"
    CACHE STRING "Linker flags for ASan builds.")
set(CMAKE_SHARED_LINKER_FLAGS_ASAN
    "-fsanitize=address"
    CACHE STRING "Shared-linker flags for ASan builds.")

# Thread Sanitizer.
set(CMAKE_C_FLAGS_TSAN
    "-fsanitize=thread -g -O1"
    CACHE STRING "Flags used by the C compiler during TSan builds.")
set(CMAKE_CXX_FLAGS_TSAN
    "-fsanitize=thread -g -O1"
    CACHE STRING "Flags used by the C++ compiler during TSan builds.")
set(CMAKE_EXE_LINKER_FLAGS_TSAN
    "-fsanitize=thread"
    CACHE STRING "Linker flags for TSan builds.")
set(CMAKE_SHARED_LINKER_FLAGS_TSAN
    "-fsanitize=thread"
    CACHE STRING "Shared-linker flags for TSan builds.")

# Undefined Behavior Sanitizer.
set(CMAKE_C_FLAGS_UBSAN
    "-fsanitize=undefined -fno-omit-frame-pointer -g -O1"
    CACHE STRING "Flags used by the C compiler during UBSan builds.")
set(CMAKE_CXX_FLAGS_UBSAN
    "-fsanitize=undefined -fno-omit-frame-pointer -g -O1"
    CACHE STRING "Flags used by the C++ compiler during UBSan builds.")
set(CMAKE_EXE_LINKER_FLAGS_UBSAN
    "-fsanitize=undefined"
    CACHE STRING "Linker flags for UBSan builds.")
set(CMAKE_SHARED_LINKER_FLAGS_UBSAN
    "-fsanitize=undefined"
    CACHE STRING "Shared-linker flags for UBSan builds.")

# Coverage.
set(CMAKE_C_FLAGS_COVERAGE
    "--coverage -g -O0"
    CACHE STRING "Flags used by the C compiler during Coverage builds.")
set(CMAKE_CXX_FLAGS_COVERAGE
    "--coverage -g -O0"
    CACHE STRING "Flags used by the C++ compiler during Coverage builds.")
set(CMAKE_EXE_LINKER_FLAGS_COVERAGE
    "--coverage"
    CACHE STRING "Linker flags for Coverage builds.")

# Mark the per-config flag vars advanced so cmake-gui doesn't
# clutter its top-level view with them.
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
