/* direct_app: links the SHARED foo directly */
int foo(void);
int main(void) { return foo() == 42 ? 0 : 1; }
