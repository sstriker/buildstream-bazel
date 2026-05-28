// Consumer-fidelity test for nlohmann/json.
//
// nlohmann/json is INTERFACE-only (header-only); there's no
// static archive on either side to library-diff. The consumer
// .o is the only meaningful artifact — its template
// instantiations + exception/std::string machinery reflects
// what downstream code linking against either build sees.
//
// Compiled twice: once against cmake's installed nlohmann/json
// headers (under <install>/include/nlohmann/), once via Bazel as
// a cc_library depending on the converted :nlohmann_json
// cc_library (which PR #268's INTERFACE-library lift now emits
// from the trace's add_library(nlohmann_json INTERFACE) call).
// fidelity-compare diffs the two .o's, classifying inlining /
// hardening differences as benign.
#include <nlohmann/json.hpp>
#include <string>

std::string run_json_smoke(const std::string& name, int age) {
    nlohmann::json j;
    j["name"] = name;
    j["age"] = age;
    j["tags"] = nlohmann::json::array({"a", "b", "c"});
    return j.dump();
}
