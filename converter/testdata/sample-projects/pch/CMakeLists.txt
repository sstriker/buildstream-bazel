cmake_minimum_required(VERSION 3.20)
project(pchsample CXX)

# The declaring shape: core's TUs are compiled as if pch.h and <vector>
# were #included first (cmake force-includes the generated cmake_pch.hxx).
# -Werror + -Wunused-const-variable is the warning-suppression probe:
# pch.h deliberately defines an unused const that warns when included
# DIRECTLY, but cmake's cmake_pch.hxx carries #pragma GCC system_header,
# so the warning is suppressed — the converted build must reproduce that
# (the mirror header carries the same pragma) or -Werror breaks it.
add_library(core STATIC core.cpp)
target_precompile_headers(core PRIVATE pch.h <vector>)
target_compile_options(core PRIVATE -Wunused-const-variable -Werror)

# The REUSE_FROM shape: user shares core's precompiled header. Its
# codemodel carries NO precompileHeaders entry — the PCH reaches it only
# as a `-include .../core.dir/cmake_pch.hxx` compile fragment.
add_library(user STATIC user.cpp)
target_precompile_headers(user REUSE_FROM core)

# The per-TU opt-out shape: skip.cpp compiles WITHOUT the PCH
# (SKIP_PRECOMPILE_HEADERS) — it must NOT see PCH_PROVIDED.
add_library(mixed STATIC mixed.cpp skip.cpp)
target_precompile_headers(mixed PRIVATE pch.h)
set_source_files_properties(skip.cpp PROPERTIES SKIP_PRECOMPILE_HEADERS ON)
