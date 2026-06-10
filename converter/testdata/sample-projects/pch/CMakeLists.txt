cmake_minimum_required(VERSION 3.20)
project(pchsample CXX)

# The declaring shape: core's TUs are compiled as if pch.h and <vector>
# were #included first (cmake force-includes the generated cmake_pch.hxx).
add_library(core STATIC core.cpp)
target_precompile_headers(core PRIVATE pch.h <vector>)

# The REUSE_FROM shape: user shares core's precompiled header. Its
# codemodel carries NO precompileHeaders entry — the PCH reaches it only
# as a `-include .../core.dir/cmake_pch.hxx` compile fragment.
add_library(user STATIC user.cpp)
target_precompile_headers(user REUSE_FROM core)
