cmake_minimum_required(VERSION 3.20)
project(cons LANGUAGES C VERSION 0.1.0)

# Consumer kind:cmake element. Depends on `auto-prod`, a
# kind:autotools element. At convert-element time, write-a's
# cmake handler stages auto-prod's PUBLISHED cmake-config bundle
# under $PREFIX/lib/cmake/autoprod/ — the bundle bytes came from
# the AC via :auto-prod_trace_load's --out-config-bundle output
# (the cross-element configure-step bootstrap rendezvous; see
# docs/design/rendezvous.md).
#
# find_package(autoprod CONFIG REQUIRED) below resolves against
# that bundle. Before PR-4 this `find_package` would have failed:
# the legacy cmakeDepBundleLabels filtered to kind=cmake deps,
# silently skipping auto-prod, leaving $PREFIX empty for the
# autoprod package.
find_package(autoprod CONFIG REQUIRED)

add_library(cons STATIC src/cons.c)
target_include_directories(cons PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
target_link_libraries(cons PUBLIC autoprod::autoprod)

install(TARGETS cons
    ARCHIVE DESTINATION lib
    LIBRARY DESTINATION lib)
