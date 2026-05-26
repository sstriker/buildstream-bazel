cmake_minimum_required(VERSION 3.20)
project(platform_partition_tier2 LANGUAGES C VERSION 0.1.0)

# Platform-conditional source partitioning fixture covering
# both Tier 1 (#217) and Tier 2 (#217 follow-on).
#
# Tier 1: the platform's own conditional sources surface from
# the trace alone. On a Linux configure, cmake traces the
# `if(LINUX) target_sources(app PRIVATE linux.c) endif()` body
# and the lower partitions linux.c into the linux arm of the
# emitted select().
#
# Tier 2: the OTHER arm (elseif(WIN32) target_sources(app
# PRIVATE win.c)) is the body cmake didn't enter — it never
# appears in the trace. Tier 2's CMakeLists parser re-reads
# this file at the recorded `if()` event's line, walks the
# elseif arm, and emits win.c under @platforms//os:windows so
# bazel can later reconfigure for Windows and pick up the
# Windows-only source.
#
# The render gate (scripts/meta-cmake-platform-partition-tier2.sh)
# asserts the emitted BUILD.bazel has BOTH:
#
#   - linux.c attributed under @platforms//os:linux (Tier 1)
#   - win.c attributed under @platforms//os:windows (Tier 2)
#
# Tier 1's existing render-side proofs (lower_test.go's
# TestToIR_PlatformConditionalSrcs_* suite) cover the in-trace
# half; this gate is the new capability — recovery of a
# source cmake never executed.

add_library(app STATIC base.c)

if(LINUX)
    target_sources(app PRIVATE linux.c)
elseif(WIN32)
    target_sources(app PRIVATE win.c)
endif()
