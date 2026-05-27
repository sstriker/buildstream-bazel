// Consumer-fidelity test for fmt.
//
// Calls a representative subset of fmt's API; the resulting .o
// captures whatever template instantiations + inlines the consumer
// pulls in via fmt's headers. fidelity-compare diffs the two .o's
// (cmake-installed-headers compile vs Bazel-converted-target
// compile); the prefix pairer classifies inlining-decision
// differences as benign template pairs.
#include <fmt/core.h>
#include <fmt/format.h>
#include <string>

std::string run_fmt_smoke(int value, double pi) {
    return fmt::format("v={} pi={:.3f}", value, pi);
}
