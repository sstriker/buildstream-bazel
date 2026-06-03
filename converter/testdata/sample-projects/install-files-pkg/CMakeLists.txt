cmake_minimum_required(VERSION 3.20)
project(installfilespkg LANGUAGES C VERSION 0.1.0)

# Fixture for Phase 1 slice 1b: install(FILES ... DESTINATION ...) and
# install(DIRECTORY ... DESTINATION ...) lower to rules_pkg's
# pkg_files at convert time, carrying the install DESTINATION as the
# pkg_files `prefix` attribute (instead of an opaque filegroup that
# loses the destination). The converter consumes the File API
# Directory.Installers entries for the "file" / "directory" installer
# types; no cmake --install runs.

add_library(greeter STATIC greeter.c)
target_include_directories(greeter PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)

# install(FILES) — explicit files land under share/installfilespkg.
install(FILES data/greeting.txt data/farewell.txt
        DESTINATION share/installfilespkg)

# install(FILES) — a public header lands under include.
install(FILES include/greeter.h DESTINATION include)

# install(FILES ... RENAME ...) — the File API records the renamed file
# as a {"from","to"} object; the lowering lifts "to" onto the pkg_files
# `renames` map so the file lands at share/renamed/GREETING (not
# share/renamed/greeting.txt). Before this was modeled the object-form
# entry was dropped and the file silently vanished from the package.
install(FILES data/greeting.txt RENAME GREETING DESTINATION share/renamed)

# install(DIRECTORY) trailing slash — CONTENTS of docs/ land directly
# under share/doc (share/doc/README.txt). Recorded as the
# {"from":"docs","to":"."} object; strips the whole dir.
install(DIRECTORY docs/ DESTINATION share/doc)

# install(DIRECTORY) NO trailing slash — the include DIR ITSELF lands
# under include-tree (include-tree/include/greeter.h). Recorded as a
# plain string; strips only the dir's parent so the dir name survives.
install(DIRECTORY include DESTINATION include-tree)
