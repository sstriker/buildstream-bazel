# Write the REAL generated sources as an UNDECLARED side effect into OUTPUT_DIR
# (a -D cache arg). The ninja edge declares only MANIFEST as its OUTPUT, so foo.c
# / foo.h are consumed orphans with no producer edge. OUTPUT_DIR / MANIFEST arrive
# as -D args from the custom command.
file(WRITE ${OUTPUT_DIR}/foo.c "#include \"foo.h\"\nint gen_value(void) { return 7; }\n")
file(WRITE ${OUTPUT_DIR}/foo.h "int gen_value(void);\n")
file(WRITE ${MANIFEST} "# manifest\n")
