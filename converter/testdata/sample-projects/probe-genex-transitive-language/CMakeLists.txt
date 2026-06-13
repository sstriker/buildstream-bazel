cmake_minimum_required(VERSION 3.24)
project(probe_genex_transitive_language C CXX)

# TRANSITIVE language-conditional gate: the dependency carries the
# $<COMPILE_LANGUAGE:…> usage requirement on its INTERFACE; the consumer
# links it PUBLIC, so cmake's
# $<TARGET_PROPERTY:consumer,INTERFACE_COMPILE_OPTIONS> aggregates the
# dep's gated value at generation time. The structural genex probe's
# file(GENERATE) for the consumer then diverges per enabled language
# (multi-language project) and fatally aborts — even though the
# consumer's OWN raw interface is clean. The direct-value skip misses
# this; the probe's link-closure walk (_cmtb_iface_lang_gate) catches it.
add_library(dep INTERFACE)
target_compile_options(dep INTERFACE
  $<$<COMPILE_LANGUAGE:CXX>:-fno-exceptions>)
target_compile_definitions(dep INTERFACE
  PLAIN=1
  $<$<COMPILE_LANGUAGE:CXX>:CXXONLY=1>)

# Consumer's own interface is CLEAN — the gate arrives only through the
# PUBLIC link to dep.
add_library(consumer STATIC consumer.c)
target_link_libraries(consumer PUBLIC dep)

# A second-level consumer to exercise the multi-hop closure.
add_library(top STATIC top.c)
target_link_libraries(top PUBLIC consumer)
