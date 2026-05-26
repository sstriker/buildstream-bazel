#include <zlib.h>
#include "usepkg_var.h"

int usepkg_var_run(void) {
    return zlibVersion()[0] != '\0';
}
