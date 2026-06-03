cmake_minimum_required(VERSION 3.20)
project(exporthdr CXX)

# A project that uses CMake's GenerateExportHeader module — the near-universal
# cmake idiom for symbol-visibility export macros. generate_export_header()
# calls configure_file() from CMake's own module dir against the fixed template
# exportheader.cmake.in, producing <name>_export.h. The converter must recover
# that header (its call-site is outside the project tree) AND wire its
# directory onto the include path so the BARE `#include "mylib_export.h"`
# resolves — even under --split-packages, where the header lands in a subdir.
add_subdirectory(lib)
