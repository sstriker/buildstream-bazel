#ifdef PARTIAL_FAST
int partial_impl(void) { return 2; }
#else
int partial_impl(void) { return 1; }
#endif
