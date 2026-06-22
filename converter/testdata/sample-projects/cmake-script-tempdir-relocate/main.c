/* gen_value() is defined in the generated source the cmake -P wrapper's tool
 * wrote into a temp dir and `cmake -E copy`'d to the declared output. */
extern int gen_value(void);

int main(void) {
	return gen_value() == 7 ? 0 : 1;
}
