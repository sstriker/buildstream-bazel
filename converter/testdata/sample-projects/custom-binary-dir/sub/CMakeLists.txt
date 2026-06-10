configure_file(cfg.h.in cfg.h)
file(GENERATE OUTPUT gen.h CONTENT "#define GEN 1\n")
add_library(uses STATIC uses.c)
target_include_directories(uses PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
