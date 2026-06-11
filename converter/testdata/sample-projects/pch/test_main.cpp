// Include-free: compiles only through the forced include (the mirror).
#if !PCH_PROVIDED
#error "pch was not force-included"
#endif
int main() { return std::vector<std::string>{std::string("t")}.size() == 1 ? 0 : 1; }
