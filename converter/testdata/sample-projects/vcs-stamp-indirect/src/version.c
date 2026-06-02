#include "version.h"

/* The revision arrives via set(VERSION ${GIT_SHA}); under --stamp the
   converted build re-reads it from the workspace status. */
const char *vcsstampindirect_version(void) { return VCSSTAMPINDIRECT_VERSION; }
