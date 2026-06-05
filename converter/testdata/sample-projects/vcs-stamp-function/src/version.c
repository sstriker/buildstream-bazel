#include "version.h"

/* The revision arrives via a helper function's set(${_var} "${out}"
   PARENT_SCOPE) return (git_describe()'s shape); the converter recovers the
   forwarded copy so the stamp call lifts instead of aborting the convert. */
const char *vcsstampfunction_version(void) { return VCSSTAMPFUNCTION_VERSION; }
