/* Both generated sources are written by tools (gen_a.sh / gen_b.sh) into
 * sibling subdirs gen/a and gen/b, as undeclared side effects of two separate
 * cmake -P stamp edges. */
#include "foo.h"
#include "bar.h"

int main(void) {
	return (foo_value() == 7 && bar_value() == 11) ? 0 : 1;
}
