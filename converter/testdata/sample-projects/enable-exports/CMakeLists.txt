cmake_minimum_required(VERSION 3.24)
project(enable_exports LANGUAGES C)

# An executable that exports its dynamic symbol table so dlopen'd
# plugins can resolve against it. cmake implements ENABLE_EXPORTS by
# adding the platform export-dynamic linker flag (-rdynamic /
# -Wl,--export-dynamic on GNU/Clang ld). The converter lifts this to
# the native Bazel linkopts entry rather than only tagging the gap.
add_executable(host_app src/main.c)
set_target_properties(host_app PROPERTIES ENABLE_EXPORTS TRUE)
