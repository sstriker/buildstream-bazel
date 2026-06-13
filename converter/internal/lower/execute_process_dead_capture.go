package lower

import (
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Dead-capture analysis: an execute_process capture keyword
// (OUTPUT_VARIABLE / ERROR_VARIABLE / RESULT_VARIABLE /
// RESULTS_VARIABLE) whose variable the configure NEVER READS afterward
// exists only to silence console output — the ubiquitous
// `OUTPUT_VARIABLE _quiet ERROR_VARIABLE _quiet` idiom. Several refusal
// classes trigger on the keyword being PRESENT (unrecognized-driver
// captures, the nested-cmake capture refusal, the stamp/probe
// capture gate), so a silencing capture historically killed lifts the
// call otherwise qualified for — in strict mode, the whole conversion.
//
// The loop: pass 1 records every capture-bearing refusal's variable
// names into Options.CaptureRefusalSink; the driver then runs a warm
// NON-EXPANDED trace pass (the same instrument as the stamp set-copy
// recovery — the expanded trace substitutes `${var}` away, so reads are
// only visible verbatim), computes the read set
// (shadow.ExtractVariableReads), and re-lowers with
// Options.DeadCaptureVars = sink − reads. clearDeadCaptures then
// normalizes each call BEFORE classification, so every downstream
// classifier and lifter sees the silenced call as if the keyword were
// absent. The original call's vars still re-record on any remaining
// refusal — already-dead vars stay dead, so the second pass converges.
//
// Conservatism: ExtractVariableReads over-counts reads (keywords,
// operators), and a capture read through VARIABLE INDIRECTION can look
// dead (see its doc); a falsely-dead capture would drop a value that
// steered configure flow — but the steering's RESULT is already
// materialized in the codemodel/rendered outputs this same conversion
// consumed, so the blast radius is the lost refusal diagnostic, not a
// wrong build.

// clearDeadCaptures returns the call with every capture channel whose
// variable is provably dead cleared, as if the keyword were absent.
func clearDeadCaptures(call shadow.ExecuteProcessCall, dead map[string]bool) shadow.ExecuteProcessCall {
	if len(dead) == 0 {
		return call
	}
	if dead[call.OutputVariable] {
		call.OutputVariable = ""
	}
	if dead[call.ErrorVariable] {
		call.ErrorVariable = ""
	}
	if dead[call.ResultVariable] {
		call.ResultVariable = ""
	}
	if dead[call.ResultsVariable] {
		call.ResultsVariable = ""
	}
	return call
}

// recordCaptureRefusal records a refused call's capture variables into
// the sink so the driver knows a dead-capture pass could change the
// outcome. Recording the ORIGINAL (pre-clear) call keeps the signal
// complete; nil sink (no driver pass wired, unit tests) is a no-op.
func recordCaptureRefusal(call shadow.ExecuteProcessCall, cc *codegenContext) {
	if cc.CaptureRefusalSink == nil {
		return
	}
	for _, v := range []string{call.OutputVariable, call.ErrorVariable, call.ResultVariable, call.ResultsVariable} {
		if v != "" {
			cc.CaptureRefusalSink[v] = true
		}
	}
}

// callCaptureCleared reports whether dead-capture normalization removed
// any capture channel from the call (orig is the pre-clear form).
func callCaptureCleared(orig, cleared shadow.ExecuteProcessCall) bool {
	return orig.OutputVariable != cleared.OutputVariable ||
		orig.ErrorVariable != cleared.ErrorVariable ||
		orig.ResultVariable != cleared.ResultVariable ||
		orig.ResultsVariable != cleared.ResultsVariable
}
