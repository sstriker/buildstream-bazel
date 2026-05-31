cmake_minimum_required(VERSION 3.20)
project(probe_genex_dangling_link C)

# A real, buildable target — must still get fully probed (the fix is
# selective: only the pathological target below is skipped).
add_library(real STATIC real.c)

# A non-TESTONLY INTERFACE library whose link interface names a
# namespaced (::) target that was never created. This is abseil's
# `heterogeneous_lookup_testing` -> `absl::test_instance_tracker` shape:
# a non-TESTONLY helper that DEPS a TESTONLY lib which abseil skips
# creating when testing is off, leaving a dangling `::` reference.
#
# cmake tolerates the dangling ref at the target_link_libraries call and
# until something forces the interface to evaluate. probe-genex's
# `file(GENERATE $<TARGET_PROPERTY:t,INTERFACE_LINK_LIBRARIES>)` would
# force it and fatal-error the whole generate step ("the target was not
# found"). The fix skips probing any target with such a dangling `::`
# dep; this fixture pins that.
add_library(dangling_consumer INTERFACE)
target_link_libraries(dangling_consumer INTERFACE absl::nonexistent_testonly)
