// SKIP_PRECOMPILE_HEADERS: this TU must NOT see the PCH.
#ifdef PCH_PROVIDED
#error "pch leaked into a SKIP_PRECOMPILE_HEADERS source"
#endif
int skip_marker() { return 1; }
