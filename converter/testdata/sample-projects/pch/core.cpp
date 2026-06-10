// Deliberately include-free: this TU compiles ONLY because the PCH is
// force-included first (std::string via pch.h's <string>, std::vector via
// the <vector> PCH entry, PCH_PROVIDED via pch.h). That reliance is the
// regression this fixture guards — dropping the forced include instead of
// lifting it breaks the compile.
#if !PCH_PROVIDED
#error "pch was not force-included"
#endif
std::vector<std::string> core_names() { return {std::string("core")}; }
