package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Lifting a configure-time *feature* probe into a Bazel build setting.
//
// A probe like `execute_process(COMMAND check-thing RESULT_VARIABLE
// HAVE_THING)` is a deferred declaration: it records whether the host
// has THING so cmake can branch on it. The writeback channel decides how
// the captured value reads:
//
//   - RESULT_VARIABLE is the command's EXIT STATUS — "0" means success,
//     i.e. the feature IS present (the consumer tests `if(HAVE_THING
//     EQUAL 0)` / `if(NOT HAVE_THING)`). This is the INVERSE of cmake's
//     string-truthiness, so "0" lifts to a default-True flag.
//   - OUTPUT_VARIABLE is stdout — a string cmake's if() tests directly,
//     so it runs through cmakeTruthy (1/ON/YES/TRUE/Y true, else false).
//
// The faithful Bazel translation is neither a refusal nor a silent
// bake — it's a declared, operator-overridable build setting (a
// bazel_skylib `bool_flag`) plus a `config_setting` consumers can
// `select()` on. recoverExecuteProcess emits that pair — defaulting
// True when the captured value says the feature was present (see
// featureProbeDefault), else False — and skips the probe.

// featureProbeNamePrefixes / featureProbeNameSuffixes name the variable
// shapes a boolean feature probe writes — what an if() later gates a
// define / option on.
var featureProbeNamePrefixes = []string{"HAVE_", "ENABLE_", "USE_", "WITH_"}
var featureProbeNameSuffixes = []string{"_FOUND", "_SUPPORTED", "_AVAILABLE"}

// featureDeclarationProbeVar returns the probe's writeback variable when
// its name reads as a boolean feature declaration the converter can lift
// to a build setting, along with whether that variable came from
// RESULT_VARIABLE (an exit status, "0" == success — the inverse of
// cmakeTruthy) rather than OUTPUT_VARIABLE (a stdout string). Returns
// ("", false) when neither channel carries a feature-shaped name.
// OUTPUT_VARIABLE wins when both are feature-shaped: a captured stdout
// string is the more direct declaration than an exit status.
func featureDeclarationProbeVar(call shadow.ExecuteProcessCall) (name string, fromResult bool) {
	if call.OutputVariable != "" && isFeatureDeclarationVar(call.OutputVariable) {
		return call.OutputVariable, false
	}
	if call.ResultVariable != "" && isFeatureDeclarationVar(call.ResultVariable) {
		return call.ResultVariable, true
	}
	return "", false
}

// isFeatureDeclarationVar reports whether a cmake variable name reads as
// a boolean feature/capability declaration (HAVE_*, ENABLE_*, USE_*,
// WITH_*, *_FOUND, *_SUPPORTED, *_AVAILABLE).
func isFeatureDeclarationVar(name string) bool {
	u := strings.ToUpper(name)
	for _, p := range featureProbeNamePrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	for _, s := range featureProbeNameSuffixes {
		if strings.HasSuffix(u, s) {
			return true
		}
	}
	return false
}

// sanitizeBuildSettingName lowercases a cmake variable into a
// Bazel-target-name-safe build-setting name (HAVE_BACKTRACE ->
// have_backtrace).
func sanitizeBuildSettingName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// featureProbeDefault derives the bool_flag's build_setting_default from
// the captured probe value, branching on the writeback channel:
//
//   - RESULT_VARIABLE (fromResult): the value is the command's exit
//     status, where "0" == success == feature present. So default True
//     iff the value is exactly "0". An uncaptured value ("" — dump-vars
//     off, or a function-local var the hook missed) is not "0", so it
//     defaults False: conservative "feature off until the operator opts
//     in." A non-zero captured exit ("1", "127") likewise defaults False.
//   - OUTPUT_VARIABLE: the value is stdout, tested the way cmake's if()
//     tests any string — through cmakeTruthy (lower.go), the package's
//     shared cmake-boolean predicate (1/ON/YES/TRUE/Y true, else false),
//     so numeric-zero strings like "0.0" resolve false, matching cmake.
//
// Either way the operator can flip the lifted flag with
// --//pkg:have_x=True/False; the default just seeds the common case.
func featureProbeDefault(value string, fromResult bool) bool {
	if fromResult {
		return value == "0"
	}
	return cmakeTruthy(value)
}
