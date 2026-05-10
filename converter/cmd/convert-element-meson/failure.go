// Tier-1 failure codes specific to the meson converter. We extend the
// shared `failure.Code` taxonomy here rather than reusing the cmake
// codes — the orchestrator's regression detector keys off the code
// string, so meson-specific failures need distinct names.
//
// See docs/failure-schema.md for the canonical enumeration.
package main

import "github.com/sstriker/cmake-to-bazel/converter/internal/failure"

const (
	mesonSetupFailed                 failure.Code = "meson-setup-failed"
	unsupportedMesonTargetType       failure.Code = "unsupported-meson-target-type"
	unsupportedMesonSubproject       failure.Code = "unsupported-meson-subproject"
	unsupportedMesonCustomTarget     failure.Code = "unsupported-meson-custom-target"
	unsupportedMesonGeneratedSources failure.Code = "unsupported-meson-generated-sources"
	unsupportedMesonCrossCompile     failure.Code = "unsupported-meson-cross-compile"
	unresolvedMesonDependency        failure.Code = "unresolved-meson-dependency"
)

func newFailure(code failure.Code, format string, args ...any) *failure.Error {
	return failure.New(code, format, args...)
}
