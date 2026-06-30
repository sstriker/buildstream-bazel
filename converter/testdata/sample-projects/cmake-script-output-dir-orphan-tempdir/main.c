/* foo_value() is declared in the generated header foo.h and defined in the
 * generated source foo.c — both written by tool.py into a temp dir and
 * file(COPY)'d into OUTPUT_DIR by the cmake -P wrapper, as undeclared side
 * effects of the stamp edge. */
#include "foo.h"

int main(void) {
	return foo_value() == 7 ? 0 : 1;
}
