cmake_minimum_required(VERSION 3.16)
project(optionlift C)

option(FOO_FEATURE "Enable the feature codepath" ON)

add_library(foo STATIC common.c)
if(FOO_FEATURE)
  target_compile_definitions(foo PRIVATE FOO_FEATURE=1)
  target_sources(foo PRIVATE feature.c)
endif()
