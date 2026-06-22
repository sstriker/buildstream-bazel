# cmake -P script: file(WRITE)s the generated source AND the recipe that
# target_sources()'s it onto the outer `app`. Content-only (no inner tool), so
# the faithful recovery under --cmake-script-bake is a write_file producer (no
# cmake -P at Bazel build time). GENDIR is passed via -D.
file(WRITE ${GENDIR}/type_a.c "int gen_value(void) { return 7; }\n")
file(WRITE ${GENDIR}/recipe.cmake "target_sources(app PRIVATE ${GENDIR}/type_a.c)\n")
