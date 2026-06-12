# Added twice by the top-level CMakeLists with a per-add LIBNAME, the
# classic "same source dir built into two binary dirs" vendoring shape.
add_library(${LIBNAME} STATIC lib.c)
