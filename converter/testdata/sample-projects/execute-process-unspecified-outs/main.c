#include "data_gen.h"

int gen_value(void);

int main(void) { return (gen_value() == 42 && DATA_GEN == 7) ? 0 : 1; }
