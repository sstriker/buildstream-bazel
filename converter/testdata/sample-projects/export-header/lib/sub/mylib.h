#ifndef MYLIB_H
#define MYLIB_H

// Bare-name include of the generate_export_header output (resolved via
// CMAKE_CURRENT_BINARY_DIR/sub on the include path).
#include "mylib_export.h"

MYLIB_EXPORT int mylib_answer();

#endif
