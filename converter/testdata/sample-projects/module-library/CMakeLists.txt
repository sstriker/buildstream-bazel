cmake_minimum_required(VERSION 3.20)
project(module_library C)

# A cmake MODULE library: a plugin dlopen'd at runtime, NEVER linked by a
# consumer. The whole point of this fixture is the dlopen-by-EXACT-name
# contract — cmake names it `libplugin.so` (no soversion), and the host
# loads it with dlopen("libplugin.so"). Faithful SHARED conversion must
# emit the cc_shared_library with that exact name (not the `.so.1` suffix
# the SHARED path uses to dodge its impl-collision), or dlopen can't find it.
add_library(plugin MODULE plugin.c)

# The loader. It links libdl (${CMAKE_DL_LIBS}) but does NOT link the module
# — there is no target_link_libraries(host plugin); cmake has no static record
# of which executable dlopens which module (it's a runtime string), so the
# converter cannot and does not auto-wire the .so into the host's deps.
add_executable(host host.c)
target_link_libraries(host PRIVATE ${CMAKE_DL_LIBS})
