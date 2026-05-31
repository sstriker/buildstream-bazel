# Synthetic "Darwin/macOS" toolchain for the multi-platform survey
# (scripts/survey-multiplatform.sh). Same idea as windows.cmake: make
# cmake evaluate the project's `if(APPLE)` /
# `if(CMAKE_SYSTEM_NAME STREQUAL "Darwin")` branches so the codemodel +
# trace carry the macOS-arm sources, WITHOUT a real cross-compiler. The
# emitted artefacts are never built — only the File API reply + trace are
# consumed.
set(CMAKE_SYSTEM_NAME Darwin)
set(CMAKE_SYSTEM_PROCESSOR arm64)

# Skip the compiler-works check (no real Apple clang here).
set(CMAKE_C_COMPILER_WORKS TRUE)
set(CMAKE_CXX_COMPILER_WORKS TRUE)
set(CMAKE_C_COMPILER_FORCED TRUE)
set(CMAKE_CXX_COMPILER_FORCED TRUE)
# Apple Clang so APPLE + the Clang predicate arms evaluate; the converter
# maps APPLE to @platforms//os:darwin.
set(CMAKE_C_COMPILER_ID AppleClang)
set(CMAKE_CXX_COMPILER_ID AppleClang)

# CMakeFindBinUtils looks for Apple-specific tools (install_name_tool,
# libtool, etc.) that don't exist on a Linux host and aborts configure.
# Point them at /usr/bin/true — they're never invoked (nothing is built),
# we only need configure + generation to complete for the codemodel.
set(CMAKE_INSTALL_NAME_TOOL /usr/bin/true CACHE FILEPATH "")
set(CMAKE_LINKER /usr/bin/true CACHE FILEPATH "")
set(CMAKE_AR /usr/bin/true CACHE FILEPATH "")
set(CMAKE_RANLIB /usr/bin/true CACHE FILEPATH "")
set(CMAKE_STRIP /usr/bin/true CACHE FILEPATH "")
set(CMAKE_NM /usr/bin/true CACHE FILEPATH "")
set(CMAKE_OBJDUMP /usr/bin/true CACHE FILEPATH "")
set(CMAKE_LIBTOOL /usr/bin/true CACHE FILEPATH "")
set(CMAKE_OTOOL /usr/bin/true CACHE FILEPATH "")
set(CMAKE_DSYMUTIL /usr/bin/true CACHE FILEPATH "")
