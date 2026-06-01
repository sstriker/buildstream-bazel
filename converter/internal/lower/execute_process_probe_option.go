package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Lifting a configure-time *feature* probe into a Bazel build setting.
//
// A probe like `execute_process(COMMAND check-thing RESULT_VARIABLE
// HAVE_THING)` is a deferred declaration: it asks "does the host have
// THING?" so cmake can branch on HAVE_THING. The faithful Bazel
// translation is neither a refusal nor a silent bake — it's a declared,
// operator-overridable build setting (a bazel_skylib `bool_flag`) plus a
// `config_setting` consumers can `select()` on. recoverExecuteProcess
// emits that pair (default off the captured value when the dump-vars
// hook caught it, else false) and skips the probe.

// featureProbeNamePrefixes / featureProbeNameSuffixes name the variable
// shapes a boolean feature probe writes — what an if() later gates a
// define / option on.
var featureProbeNamePrefixes = []string{"HAVE_", "ENABLE_", "USE_", "WITH_"}
var featureProbeNameSuffixes = []string{"_FOUND", "_SUPPORTED", "_AVAILABLE"}

// featureDeclarationProbeVar returns the probe's writeback variable
// (OUTPUT_VARIABLE or RESULT_VARIABLE) when its name reads as a boolean
// feature declaration the converter can lift to a build setting, else "".
func featureDeclarationProbeVar(call shadow.ExecuteProcessCall) string {
	for _, v := range []string{call.OutputVariable, call.ResultVariable} {
		if v != "" && isFeatureDeclarationVar(v) {
			return v
		}
	}
	return ""
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

// isTruthyCMakeValue maps a cmake variable value to a bool the way if()
// does: 1 / ON / YES / TRUE / a non-empty, non-zero, non-NOTFOUND value
// is true.
func isTruthyCMakeValue(v string) bool {
	u := strings.ToUpper(strings.TrimSpace(v))
	switch u {
	case "", "0", "OFF", "NO", "FALSE", "N", "IGNORE", "NOTFOUND":
		return false
	}
	return !strings.HasSuffix(u, "-NOTFOUND")
}
