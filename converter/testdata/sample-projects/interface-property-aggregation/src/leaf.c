#include <base/base.h>

int leaf_compute(void) {
    /* FOO comes from base's INTERFACE_COMPILE_DEFINITIONS. */
    return base_value() + FOO;
}

int base_value(void) {
    return 0;
}
