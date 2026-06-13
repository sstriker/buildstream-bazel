#include <string.h>
#include "greeting.h"
int sub_value(void);
int main(void) { return (sub_value() == 5 && strcmp(GREETING_WHO, "world") == 0) ? 0 : 1; }
