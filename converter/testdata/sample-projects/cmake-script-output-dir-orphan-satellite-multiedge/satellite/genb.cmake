# Write a shared header DIRECTLY into the component dir (the level the parent
# expansion leaks). shared.h is a cross-boundary consumed orphan the OUTER app
# includes; manifest_b.cmake is the edge's declared stamp.
file(WRITE ${OUTPUT_DIR}/shared.h "#define SHARED_BONUS 0\n")
file(WRITE ${OUTPUT_DIR}/manifest_b.cmake "# stamp b\n")
