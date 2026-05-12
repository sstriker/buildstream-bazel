#include "version.h"
#include "banner.h"
#include "config_tag.h"

const char *fglib_version(void) { return FGLIB_VERSION; }
const char *fglib_banner(void) { return BANNER; }
const char *fglib_build_tag(void) { return BUILD_TAG; }
