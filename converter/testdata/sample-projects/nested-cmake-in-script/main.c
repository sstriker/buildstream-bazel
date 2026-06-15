int sub_value(void);
/* sub_value() returns the nested configure's SUB_VALUE=42 (sub.c includes the
 * nested configure-generated sub_config.h), so exit 0 proves the whole nested
 * build lifted: the merged sublib linked and the baked header carried 42. */
int main(void) { return sub_value() == 42 ? 0 : 1; }
