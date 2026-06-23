/* gen_a() / gen_b() are defined in the two generated sources the cmake -P
 * wrapper's tool wrote into a temp dir and relocated to the declared outputs
 * with a single multi-source `cmake -E copy`. */
extern int gen_a(void);
extern int gen_b(void);

int main(void) {
	return gen_a() + gen_b() == 7 ? 0 : 1;
}
