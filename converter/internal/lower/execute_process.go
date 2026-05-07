package lower

import (
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/internal/shadow"
)

// recoverExecuteProcess walks the trace's execute_process
// calls, classifies each into a Bucket, and either emits a
// Bazel genrule for the liftable buckets (cmake-e,
// file-producing) or aggregates the unliftable ones into a
// single typed Tier-1 failure.
//
// The aggregation is intentional: a project with many
// execute_process calls (a common shape — version stamping +
// toolchain probing + a few tool invocations) should produce
// one triage report listing every offending call rather than N
// converter runs uncovering them one at a time. The orchestrator
// in M3 dedupes failure logs by (Code, message-prefix) so the
// per-call detail goes after a stable prefix.
//
// v1: every recognized bucket is reported as unsupported. The
// per-bucket lifters land in follow-on commits (cmake -E first,
// file-producing second). The classifier already runs and the
// reason text in the aggregated failure tells owners what kind
// of call each line was — that triage signal is useful even
// before any lifter is wired in.
func recoverExecuteProcess(calls []shadow.ExecuteProcessCall) error {
	if len(calls) == 0 {
		return nil
	}
	var unsupported []executeProcessRefusal
	for _, call := range calls {
		v := Classify(call)
		// v1: refuse every bucket. Subsequent commits will move
		// the cmake-e and file-producing buckets out of the
		// refusal path into their respective lifters.
		unsupported = append(unsupported, executeProcessRefusal{
			Loc:    formatExecuteProcessLoc(call),
			Bucket: v.Bucket,
			Reason: v.Reason,
			Argv:   formatExecuteProcessArgv(call),
		})
	}
	if len(unsupported) == 0 {
		return nil
	}
	return failure.New(failure.UnsupportedExecuteProcess,
		"%s", formatExecuteProcessRefusal(unsupported))
}

// executeProcessRefusal is the per-call refusal record used
// inside recoverExecuteProcess to assemble the aggregated
// failure message. Loc carries file:line for the source-level
// pointer; Argv carries the joined COMMAND argv (or "<n>
// stages" for pipelines) for at-a-glance triage; Bucket and
// Reason mirror the classifier output.
type executeProcessRefusal struct {
	Loc    string
	Bucket Bucket
	Reason string
	Argv   string
}

// formatExecuteProcessLoc returns "<file>:<line>" for the
// trace event's source location, or just "<file>" when the
// trace didn't record a line number (defensive — cmake's
// JSON-v1 trace always carries one in practice).
func formatExecuteProcessLoc(call shadow.ExecuteProcessCall) string {
	if call.Line > 0 {
		return call.File + ":" + itoa(call.Line)
	}
	return call.File
}

// formatExecuteProcessArgv compresses the call's COMMAND
// pipeline into a one-line string suitable for the failure
// report. Single-stage pipelines render as the joined argv;
// multi-stage pipelines render as "<n> stages: stage0 | stage1
// | ..." so the failure reader can spot them at a glance.
// Argv elements aren't shell-escaped — the report's purpose is
// triage, not re-execution; the original CMakeLists.txt is the
// re-execution source of truth.
func formatExecuteProcessArgv(call shadow.ExecuteProcessCall) string {
	if len(call.Commands) == 0 {
		return "(no COMMAND clause)"
	}
	if len(call.Commands) == 1 {
		return strings.Join(call.Commands[0], " ")
	}
	parts := make([]string, len(call.Commands))
	for i, stage := range call.Commands {
		parts[i] = strings.Join(stage, " ")
	}
	return itoa(len(call.Commands)) + " stages: " + strings.Join(parts, " | ")
}

// formatExecuteProcessRefusal renders the aggregated refusal
// list into a single message string, sorted by source location
// so the output is stable across runs (the trace's call order
// is configure-time-evaluation order which can shift with
// cmake version drift; sorting by file:line is the stable
// presentation).
func formatExecuteProcessRefusal(refusals []executeProcessRefusal) string {
	sort.Slice(refusals, func(i, j int) bool {
		return refusals[i].Loc < refusals[j].Loc
	})
	var sb strings.Builder
	sb.WriteString(itoa(len(refusals)))
	if len(refusals) == 1 {
		sb.WriteString(" execute_process call cannot be lifted natively:\n")
	} else {
		sb.WriteString(" execute_process calls cannot be lifted natively:\n")
	}
	for _, r := range refusals {
		sb.WriteString("  - ")
		sb.WriteString(r.Loc)
		sb.WriteString(" [")
		sb.WriteString(string(r.Bucket))
		sb.WriteString("] ")
		sb.WriteString(r.Reason)
		sb.WriteString("\n      argv: ")
		sb.WriteString(r.Argv)
		sb.WriteString("\n")
	}
	sb.WriteString(
		"see docs/research/cmake_analysis.md §9 for the lift-or-refuse decision tree; " +
			"unliftable elements are intended to fall through to the round-2 fallback (Phase B, not yet wired)")
	return sb.String()
}

// itoa is a tiny strconv-free integer formatter. Avoids
// pulling strconv just for the line numbers in the refusal
// report; line numbers are always small positive ints in
// cmake's JSON-v1 trace.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
