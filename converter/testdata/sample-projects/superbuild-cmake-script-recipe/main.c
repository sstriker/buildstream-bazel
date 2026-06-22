/* gen_value() is defined in the generated source the nested cmake -P recipe
 * target_sources()'d into this target. */
extern int gen_value(void);

int main(void) {
	return gen_value() == 7 ? 0 : 1;
}
