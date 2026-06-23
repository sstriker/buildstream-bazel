/* gen_value() is defined in the generated source the cmake -P wrapper's tool
 * wrote IN PLACE (its WORKING_DIRECTORY == the declared output dir, no copy). */
extern int gen_value(void);

int main(void) {
	return gen_value() == 9 ? 0 : 1;
}
