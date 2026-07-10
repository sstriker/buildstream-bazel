cmake_minimum_required(VERSION 3.16)
project(optionlift C)

option(FOO_FEATURE "Enable the feature codepath" ON)

set(BACKEND "ref" CACHE STRING "Backend implementation")
set_property(CACHE BACKEND PROPERTY STRINGS ref fast)

add_library(foo STATIC common.c)
if(FOO_FEATURE)
  target_compile_definitions(foo PRIVATE FOO_FEATURE=1)
  target_sources(foo PRIVATE feature.c)
endif()
if(BACKEND STREQUAL "fast")
  target_compile_definitions(foo PRIVATE USE_FAST_BACKEND=1)
endif()

configure_file(cfg.h.in cfg.h)

option(BUILD_EXTRA_TOOL "Build the extra tool" ON)
if(BUILD_EXTRA_TOOL)
  add_executable(extra_tool tool.c)
endif()
