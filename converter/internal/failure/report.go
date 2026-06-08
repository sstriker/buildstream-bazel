package failure

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ReportTier1 frames err on stderr prefixed by prog (the binary name) and, when
// err is a typed Tier-1 *Error, optionally writes the failure.json envelope.
//
// The envelope — {"tier":1,"code":<code>,"message":<message>} with a trailing
// newline — is the on-disk contract the orchestrator's regression detector
// parses, so it's deliberately identical across every convert-element-* binary;
// this is the single shared writer for it. It is written only when writeFailure
// is true AND outFailure is non-empty (callers pass writeFailure=false to
// suppress it — e.g. convert-element-pyproject's --probe mode, which must stay
// side-effect-free so it can't clobber a prior non-probe run's failure JSON).
//
// Returns true iff err was a Tier-1 *Error, so the caller maps it to its own
// Tier-1 vs Tier-2 exit code. A non-Tier-1 error is printed with %v and reported
// as false (the caller's Tier-2 path).
func ReportTier1(err error, prog, outFailure string, writeFailure bool) bool {
	var tier1 *Error
	if !errors.As(err, &tier1) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		return false
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", prog, tier1.Error())
	if writeFailure && outFailure != "" {
		payload, _ := json.MarshalIndent(map[string]any{
			"tier":    1,
			"code":    string(tier1.Code),
			"message": tier1.Message,
		}, "", "  ")
		_ = os.MkdirAll(filepath.Dir(outFailure), 0o755)
		_ = os.WriteFile(outFailure, append(payload, '\n'), 0o644)
	}
	return true
}
