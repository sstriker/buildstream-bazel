// Consumer-fidelity test for spdlog.
//
// Exercises a representative subset of spdlog's public logging +
// formatting API; the resulting .o captures the fmt template
// instantiations + inlines spdlog's headers pull into a consumer.
// Compiled twice (once against cmake's installed headers, once via
// Bazel against the converted :spdlog target); fidelity-compare
// diffs the two .o's. The prefix pairer + stdlib/template classifier
// fold the inlining-decision differences (spdlog's vendored fmt
// header-only instantiations) into benign pairs, the same category
// the library-side gate already allowlists.
#include <spdlog/spdlog.h>
#include <spdlog/sinks/basic_file_sink.h>
#include <string>

std::string run_spdlog_smoke(int value, double pi) {
    auto logger = spdlog::basic_logger_mt("consumer", "consumer.log");
    logger->set_level(spdlog::level::debug);
    logger->info("v={} pi={:.3f}", value, pi);
    logger->flush();
    return logger->name();
}
