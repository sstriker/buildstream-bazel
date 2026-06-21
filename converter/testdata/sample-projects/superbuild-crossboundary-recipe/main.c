/* gen_value() is defined in the generated source the nested build's recipe
 * target_sources()'d into this target — a source the nested build wrote into the
 * OUTER build tree (cross-boundary). The recovered genrule must regenerate it. */
extern int gen_value(void);

int main(void) {
	return gen_value() == 7 ? 0 : 1;
}
