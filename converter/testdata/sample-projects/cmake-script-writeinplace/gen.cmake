# Run the tool with WORKING_DIRECTORY == OUTDIR (the declared output's dir): it
# writes value.c directly there by basename, naming no final path in argv. There
# is NO relocation step. TMP/TOOL/OUTDIR arrive as -D args from the custom command.
execute_process(COMMAND python3 ${TOOL} WORKING_DIRECTORY ${OUTDIR})
