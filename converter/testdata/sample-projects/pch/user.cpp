// Same reliance as core.cpp, but via REUSE_FROM — the consumer's own
// codemodel has no precompileHeaders, so only the fragment-driven lift
// makes this compile.
#if !PCH_PROVIDED
#error "pch was not force-included"
#endif
std::string user_name() { return std::string("user"); }
