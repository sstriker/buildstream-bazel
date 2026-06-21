/* gen_value() is defined in the generated source the nested build's recipe
 * target_sources()'d into this target. */
extern int gen_value(void);

int main(void) {
	return gen_value() == 42 ? 0 : 1;
}
