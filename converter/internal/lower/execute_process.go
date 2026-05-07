package lower

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
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
// Liftable buckets append to cc.Genrules (one ir.Target per
// recovered call) and register the output path in
// cc.OutToGenrule so consumer attribution can attach the
// generated artifact to any cc target whose Includes cover the
// build-dir output. Unliftable buckets — and lift attempts
// that fail their own preconditions (e.g. cmake -E copy with
// an unresolvable input path) — fall through to the refusal
// aggregator.
func recoverExecuteProcess(calls []shadow.ExecuteProcessCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, cc *codegenContext) error {
	if len(calls) == 0 {
		return nil
	}
	var unsupported []executeProcessRefusal
	for _, call := range calls {
		v := Classify(call)
		switch v.Bucket {
		case BucketCMakeE:
			if reason, ok := liftCMakeE(call, v, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir, cc); !ok {
				unsupported = append(unsupported, executeProcessRefusal{
					Loc:    formatExecuteProcessLoc(call),
					Bucket: v.Bucket,
					Reason: reason,
					Argv:   formatExecuteProcessArgv(call),
				})
			}
		default:
			unsupported = append(unsupported, executeProcessRefusal{
				Loc:    formatExecuteProcessLoc(call),
				Bucket: v.Bucket,
				Reason: v.Reason,
				Argv:   formatExecuteProcessArgv(call),
			})
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	return failure.New(failure.UnsupportedExecuteProcess,
		"%s", formatExecuteProcessRefusal(unsupported))
}

// liftCMakeE translates a recognized cmake -E builtin call
// into a Bazel genrule and appends it to cc.Genrules. Returns
// (reason, false) when the lift can't proceed (e.g. an
// unresolvable input path) so the caller can fall back to
// refusal with a precise diagnostic instead of silently
// dropping the call.
//
// The genrule's cmd is intentionally written in plain shell
// rather than re-invoking cmake at action time: cmake itself
// isn't on the executor in a Bazel-9 + bb_clientd flow, and
// the -E builtins map cleanly to portable shell tools (touch,
// cp, mkdir, ln). cmake-codegen-cmake-e tag mirrors the
// existing add_custom_command lifter so audit queries can
// split the two cleanly even though they take different
// trace-vs-ninja paths to recover.
func liftCMakeE(call shadow.ExecuteProcessCall, v ClassifyResult, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, cc *codegenContext) (string, bool) {
	argv := call.Commands[0] // single-COMMAND guaranteed by Classify
	// cmake -E <op> <args...>; argv[0]=cmake, argv[1]=-E, argv[2]=op
	args := argv[3:]
	switch v.CMakeEOp {
	case "touch":
		return liftCMakeETouch(args, recordedBuildDir, cc)
	case "copy", "copy_if_different":
		return liftCMakeECopy(args, hostSrcDir, recordedSrcDir, recordedBuildDir, cc)
	}
	return "internal: classified as cmake-e " + v.CMakeEOp + " but no lifter wired", false
}

// liftCMakeETouch translates `cmake -E touch <path> ...` into
// a genrule per output path. cmake's touch accepts multiple
// paths and emits each as a separate genrule (one Bazel rule
// per file output); a single genrule with multi-out would
// require the consumer to reference outputs by index, which
// downstream attribution doesn't model.
//
// touch with no args is rejected (refused with a diagnostic);
// a path outside the build dir is also refused — the converter
// can't anchor it as a Bazel output.
func liftCMakeETouch(paths []string, recordedBuildDir string, cc *codegenContext) (string, bool) {
	if len(paths) == 0 {
		return "cmake -E touch with no arguments", false
	}
	for _, p := range paths {
		rel, ok := executeProcessAnchorOutput(p, recordedBuildDir)
		if !ok {
			return fmt.Sprintf("cmake -E touch path %q is not under the build dir", p), false
		}
		if _, exists := cc.OutToGenrule[rel]; exists {
			// Already recovered (e.g., the same call appears
			// multiple times in the trace from re-evaluation).
			continue
		}
		name := executeProcessGenruleName(rel)
		cc.Genrules = append(cc.Genrules, ir.Target{
			Name:        name,
			Kind:        ir.KindGenrule,
			GenruleCmd:  fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && touch "$@"`),
			GenruleOuts: []string{rel},
			Tags:        cmakeETags("touch"),
			Visibility:  []string{"//visibility:private"},
		})
		cc.OutToGenrule[rel] = name
	}
	return "", true
}

// liftCMakeECopy translates `cmake -E copy <src> <dst>` (and
// the byte-equal copy_if_different form, which differs only in
// rerun-skip semantics that don't apply to Bazel actions —
// every action gets a fresh sandbox dir).
//
// v1 supports the 2-arg form only. The N-src + 1-dst-dir form
// (`cmake -E copy a b c dst/`) is more involved (would emit one
// genrule per src-to-dst mapping) and rare in practice; refused
// with a diagnostic until a real fixture forces it.
//
// The src must resolve under the source root (so it's a real
// Bazel-tracked input) and the dst must resolve under the build
// dir (so it's a real Bazel output). Either anchor failure ends
// the lift with a descriptive reason — the caller falls back to
// refusal so the operator sees exactly which path didn't
// resolve.
func liftCMakeECopy(args []string, hostSrcDir, recordedSrcDir, recordedBuildDir string, cc *codegenContext) (string, bool) {
	if len(args) != 2 {
		return fmt.Sprintf("cmake -E copy: v1 supports the 2-arg form only (got %d args)", len(args)), false
	}
	src, dst := args[0], args[1]
	srcRel, ok := executeProcessAnchorSource(src, hostSrcDir, recordedSrcDir)
	if !ok {
		return fmt.Sprintf("cmake -E copy: source %q is not under the source root", src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, recordedBuildDir)
	if !ok {
		return fmt.Sprintf("cmake -E copy: destination %q is not under the build dir", dst), false
	}
	if _, exists := cc.OutToGenrule[dstRel]; exists {
		return "", true
	}
	name := executeProcessGenruleName(dstRel)
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        []string{srcRel},
		GenruleCmd:  fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && cp "$(location %s)" "$@"`, srcRel),
		GenruleOuts: []string{dstRel},
		Tags:        cmakeETags("copy"),
		Visibility:  []string{"//visibility:private"},
	})
	cc.OutToGenrule[dstRel] = name
	return "", true
}

// executeProcessAnchorOutput tries to resolve a recorded
// absolute path as a build-dir-relative slash path. Returns
// ("", false) when the path is relative (no anchor context) or
// resolves outside the build dir.
func executeProcessAnchorOutput(p, recordedBuildDir string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	return relativeIfInsideRelaxed(recordedBuildDir, p)
}

// executeProcessAnchorSource tries to resolve a recorded
// absolute path as a source-root-relative slash path. Returns
// ("", false) when the path is relative or resolves outside
// the source root. We accept either the host-real source path
// (the recording machine's view) OR the recorded source path
// — offline fixtures keep both consistent, but production runs
// the recorder and the converter on the same machine so
// recordedSrcDir == hostSrcDir.
func executeProcessAnchorSource(p, hostSrcDir, recordedSrcDir string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	if rel, ok := relativeIfInside(recordedSrcDir, p); ok {
		return rel, true
	}
	if hostSrcDir != "" && hostSrcDir != recordedSrcDir {
		if rel, ok := relativeIfInside(hostSrcDir, p); ok {
			return rel, true
		}
	}
	return "", false
}

// executeProcessGenruleName turns a build-dir-relative output
// path into a Bazel-rule-name-safe identifier mirroring
// configureFileGenruleName: "marker.stamp" -> "gen_marker_stamp".
// We can't share configureFileGenruleName directly because
// the configure-file recovery uses the same naming convention
// for its own genrule pool — risk of name collision when both
// recoveries land on the same package. The "exec_" prefix
// scopes the namespace.
func executeProcessGenruleName(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	var sb strings.Builder
	sb.WriteString("exec_")
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		switch {
		case (c >= 'a' && c <= 'z'),
			(c >= 'A' && c <= 'Z'),
			(c >= '0' && c <= '9'),
			c == '_':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// cmakeETags returns the cmake-codegen tag set for a recovered
// cmake -E execute_process call. cmake-codegen-driver=cmake_e
// is the audit-query handle; cmake-codegen-execute-process is
// the source-of-recovery facet (distinguishes from
// add_custom_command-driven cmake -E recoveries which carry
// the same cmake_e driver but originate in build.ninja, not
// the trace). cmake-codegen-cmake-e mirrors the existing
// genrule.go tag so existing audit queries that filter on it
// pick up execute_process-derived rules without rewording.
func cmakeETags(op string) []string {
	tags := []string{
		"cmake-codegen",
		"cmake-codegen-cmake-e",
		"cmake-codegen-driver=cmake_e",
		"cmake-codegen-execute-process",
		"cmake-codegen-execute-process-op=" + op,
	}
	sort.Strings(tags)
	return tags
}

// executeProcessRefusal is the per-call refusal record used
// inside recoverExecuteProcess to assemble the aggregated
// failure message. Loc carries file:line for the source-level
// pointer; Argv carries the joined COMMAND argv (or "<n>
// stages" for pipelines) for at-a-glance triage; Bucket and
// Reason mirror the classifier output (or, for failed lifts,
// the lifter's specific diagnostic).
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
