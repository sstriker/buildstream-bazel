#include "sub_config.h"
int sub_value(void);
int sub_extra(void);
int main(void) { return sub_value() + sub_extra() == SUB_VALUE + 35 ? 0 : 1; }
