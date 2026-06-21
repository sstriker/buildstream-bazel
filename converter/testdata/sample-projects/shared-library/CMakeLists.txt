cmake_minimum_required(VERSION 3.20)
project(shared_library C)

# A real SHARED library, consumed two ways to exercise the cc_shared_library +
# dynamic_deps wiring and — the point of this gate — that the produced .so is
# carried in runfiles so `bazel run` / `bazel test` load it at runtime:
#   - app / app_test link a STATIC mid that PUBLIC-links foo (transitive shared),
#   - direct_app links foo directly (direct shared).
add_library(foo SHARED foo.c)

add_library(mid STATIC mid.c)
target_link_libraries(mid PUBLIC foo)

add_executable(app main.c)
target_link_libraries(app PRIVATE mid)

add_executable(direct_app direct_main.c)
target_link_libraries(direct_app PRIVATE foo)

enable_testing()
add_executable(app_test main.c)
target_link_libraries(app_test PRIVATE mid)
add_test(NAME app_test COMMAND app_test)
