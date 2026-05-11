// Tier-1 failure codes specific to convert-element-pyproject.
// Schema-stable per docs/failure-schema.md — once a code is in
// this list it stays.
package main

import "github.com/sstriker/cmake-to-bazel/converter/internal/failure"

const (
	unsupportedPyprojectBackend          failure.Code = "unsupported-pyproject-backend"
	unsupportedPyprojectCExtension       failure.Code = "unsupported-pyproject-c-extension"
	unsupportedPyprojectDynamicMetadata  failure.Code = "unsupported-pyproject-dynamic-metadata"
	unsupportedPyprojectPackageDiscovery failure.Code = "unsupported-pyproject-package-discovery"
	unsupportedPyprojectEntryPoint       failure.Code = "unsupported-pyproject-entry-point"
	unresolvedPyprojectDependency        failure.Code = "unresolved-pyproject-dependency"
	pyprojectParseFailed                 failure.Code = "pyproject-parse-failed"
)

func newFailure(code failure.Code, format string, args ...any) *failure.Error {
	return failure.New(code, format, args...)
}
