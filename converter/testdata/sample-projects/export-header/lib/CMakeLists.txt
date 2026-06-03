include(GenerateExportHeader)

add_library(mylib STATIC sub/mylib.cxx)

# Generate the export header into a SUBDIR of the binary dir, mirroring VTK's
# layout (the header lands under a deeper dir than the target's package). This
# is the case that stresses the split-packages include wiring: the bare
# `#include "mylib_export.h"` needs that subdir on the include path, not just
# the package root.
generate_export_header(mylib
  EXPORT_FILE_NAME sub/mylib_export.h)

target_include_directories(mylib PUBLIC "${CMAKE_CURRENT_BINARY_DIR}/sub")
