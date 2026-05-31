# Declares a cmake_minimum_required floor below 3.5, the
# compatibility cmake 4.x removed. Under cmake 4.x this
# fatal-errors at configure with:
#   "Compatibility with CMake < 3.5 has been removed from CMake.
#    Or, add -DCMAKE_POLICY_VERSION_MINIMUM=3.5 to try configuring
#    anyway."
# — the OpenBLAS class of wild project. cmakerun.Configure detects
# this and retries once with CMAKE_POLICY_VERSION_MINIMUM=3.5, so
# the project configures. Under cmake 3.x the floor is honoured
# directly (deprecation warning only); the e2e test detects that
# and skips. See cmakerun/policy_floor_e2e_test.go.
cmake_minimum_required(VERSION 3.0)
project(policy_floor_removed LANGUAGES C VERSION 0.1.0)

add_library(pf STATIC pf.c)
