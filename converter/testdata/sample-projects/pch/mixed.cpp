// Compiles only through the forced include (PCH list: pch.h).
#if !PCH_PROVIDED
#error "pch was not force-included"
#endif
std::string mixed_name() { return std::string("mixed"); }
