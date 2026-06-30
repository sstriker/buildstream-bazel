/* gen_value() is declared in the generated header foo.h and defined in the
 * generated source foo.c — both written by gen.sh (run from the cmake -P
 * wrapper's gen.cmake) into OUTPUT_DIR, as undeclared side effects of the
 * stamp edge. */
#include "foo.h"

int main(void) {
	return gen_value() == 7 ? 0 : 1;
}
