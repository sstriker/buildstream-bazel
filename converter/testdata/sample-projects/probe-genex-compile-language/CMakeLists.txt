cmake_minimum_required(VERSION 3.24)
project(probe_genex_compile_language C CXX)

# An INTERFACE library carrying language-conditional usage
# requirements — the textbook idiom
# (`$<$<COMPILE_LANGUAGE:CXX>:...>` on a header-only lib). In a
# MULTI-LANGUAGE project these make the structural genex probe's
# file(GENERATE) content diverge per enabled language, which cmake
# rejects fatally ("Evaluation file to be written multiple times with
# different content") — aborting the whole generate step and, with
# --probe-genex default-on, the whole conversion. The fix skips the
# probe for the language-conditional property only (raw-value scan);
# consumers still carry the resolved flags via their codemodel
# compile groups, which the converted consumer below pins.
add_library(iface INTERFACE)
target_compile_definitions(iface INTERFACE
  PLAIN=1
  $<$<COMPILE_LANGUAGE:CXX>:CXXONLY=1>)
target_compile_options(iface INTERFACE
  $<$<COMPILE_LANGUAGE:CXX>:-fno-exceptions>)

add_executable(app main.cc)
target_link_libraries(app PRIVATE iface)
