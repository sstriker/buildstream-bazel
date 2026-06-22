/* gen_value() is defined in the generated source the nested cmake -P recipe
 * wrote into the OUTER build tree and target_sources()'d into this target. */
extern int gen_value(void);

int main(void) {
	return gen_value() == 7 ? 0 : 1;
}
