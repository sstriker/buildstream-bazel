// Links core (calls its symbol) AND reuses its PCH — include-free.
#if !PCH_PROVIDED
#error "pch was not force-included"
#endif
std::vector<std::string> core_names();
int main() { return core_names().size() == 1 ? 0 : 1; }
