/* gen_value() is defined in the source the nested UTILITY's tool wrote into the
 * OUTER build tree and that this target consumes directly. */
extern int gen_value(void);

int main(void) {
	return gen_value() == 7 ? 0 : 1;
}
