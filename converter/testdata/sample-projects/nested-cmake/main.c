#include "sub_config.h"
int sub_value(void);
int sub_extra(void);
/* sub_value() carries the GRANDCHILD's SUBSUB_VALUE=11 (sub.c includes
 * the depth-2 configure-generated subsub_config.h), so exit 0 proves
 * the whole superbuild CHAIN lifted: 7 + 11 + 35 == 7 + 35 + 11. */
int main(void) { return sub_value() + sub_extra() == SUB_VALUE + 35 + 11 ? 0 : 1; }
