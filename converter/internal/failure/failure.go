// Package failure carries the typed Tier-1 failure taxonomy (per-codebase
// converter errors that the orchestrator in M3 collects into the manifest's
// `excluded` block without aborting orchestration).
//
// Tier 2 (converter crashed / produced malformed output) and Tier 3
// (infrastructure errors) are not modeled here — they bubble up as generic
// errors / non-zero exit codes.
//
// See docs/failure-schema.md for the canonical enumeration. M2 must keep this
// list stable so the orchestrator's regression detector has a reliable key.
package failure

import "fmt"

// Code is a stable string identifier for the Tier-1 failure category. The
// orchestrator dedupes failure logs by (Code, message-prefix); changing a code
// silently breaks dedup.
type Code string

const (
	ConfigureFailed                Code = "configure-failed"
	FileAPIMissing                 Code = "fileapi-missing"
	FileAPIMalformed               Code = "fileapi-malformed"
	NinjaParseFailed               Code = "ninja-parse-failed"
	CTestParseFailed               Code = "ctest-parse-failed"
	UnsupportedTargetType          Code = "unsupported-target-type"
	UnsupportedCustomCommand       Code = "unsupported-custom-command"
	UnsupportedCustomCommandScript Code = "unsupported-custom-command-script"
	UnsupportedExecuteProcess      Code = "unsupported-execute-process"
	UnresolvedInclude              Code = "unresolved-include"
	UnresolvedLinkDep              Code = "unresolved-link-dep"
	UnsupportedSourcePath          Code = "unsupported-source-path"
	MissingTraceData               Code = "missing-trace-data"
	BazelCanonicalizeFailed        Code = "bazel-canonicalize-failed"
)

// Error is a typed Tier-1 failure. It satisfies the error interface, and the
// CLI marshals it to failure.json when the converter exits 1.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New builds a typed Tier-1 error.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
