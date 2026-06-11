// REUSE_FROM consumer of unit_test's PCH, include-free like the rest.
#if !PCH_PROVIDED
#error "pch was not force-included"
#endif
int main() { return 0; }
