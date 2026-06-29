/* gen_value() comes from gen.c, produced by a per-target custom command whose
 * ninja DEPFILE plumbing must be stripped from the recovered genrule. */
extern int gen_value(void);

int main(void) {
	return gen_value() == 7 ? 0 : 1;
}
