# Synthetic "Windows" toolchain for the multi-platform survey
# (scripts/survey-multiplatform.sh). The survey needs cmake to evaluate
# the project's `if(WIN32)` / `if(CMAKE_SYSTEM_NAME STREQUAL "Windows")`
# branches so the codemodel + trace carry the Windows-arm sources — it
# does NOT need to actually COMPILE for Windows. So we declare the target
# system as Windows and force cmake to accept the host compiler without a
# working-compiler probe (which would fail: no MSVC/MinGW here). The
# emitted artefacts are never built; only the File API reply + trace are
# consumed, then thrown away.
set(CMAKE_SYSTEM_NAME Windows)
set(CMAKE_SYSTEM_PROCESSOR x86_64)

# Skip the compiler-works check — we only want the generation pass.
set(CMAKE_C_COMPILER_WORKS TRUE)
set(CMAKE_CXX_COMPILER_WORKS TRUE)
set(CMAKE_C_COMPILER_FORCED TRUE)
set(CMAKE_CXX_COMPILER_FORCED TRUE)
# Present the host compiler as MSVC so WIN32 + MSVC predicate arms both
# evaluate (the converter maps both to @platforms//os:windows).
set(CMAKE_C_COMPILER_ID MSVC)
set(CMAKE_CXX_COMPILER_ID MSVC)
