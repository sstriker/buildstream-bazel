# cmake -P script: writes the generated source UP into the OUTER build tree
# (OUTER_GEN_DIR, outside this nested build) and the recipe locally. Both paths
# arrive as -D args from the custom command.
file(WRITE ${OUTER_GEN_DIR}/type_a.c "int gen_value(void) { return 7; }\n")
file(WRITE ${RECIPE} "target_sources(app PRIVATE ${OUTER_GEN_DIR}/type_a.c)\n")
