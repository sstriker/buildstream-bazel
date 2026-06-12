// Compile-time pin: both the plain and the CXX-gated interface defines
// must reach this TU; runtime exit 0 is the gate's content check.
#ifndef PLAIN
#error missing PLAIN
#endif
#ifndef CXXONLY
#error missing CXXONLY
#endif
int main() { return PLAIN + CXXONLY == 2 ? 0 : 1; }
