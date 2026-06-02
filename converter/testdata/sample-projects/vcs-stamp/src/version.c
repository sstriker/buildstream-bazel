#include "version.h"

/* Returns the VCS revision baked at configure time (cmake) or re-read
   from the Bazel workspace status under --stamp (converted). */
const char *vcsstamp_git_sha(void) { return VCSSTAMP_GIT_SHA; }
