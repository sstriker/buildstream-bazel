package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// warmRecovery is what the coalesced warm second configure recovered, threaded
// into the single re-lower.
type warmRecovery struct {
	genexResolutions map[string]cmakerun.LiteralResolution
	sets             []shadow.SetAssignment
	forwards         []shadow.ParentScopeForward
	nestedBuilds     []lower.NestedBuildInput
	// deadCaptureVars are the capture variables the non-expanded trace
	// proved the configure never reads (silencing captures); the
	// re-lower clears their execute_process capture keywords. See
	// lower/execute_process_dead_capture.go.
	deadCaptureVars map[string]bool
	recovered       bool
}

// runCoalescedWarmPass runs ONE warm reconfigure carrying the union of the
// warm-pass hooks pass 1 demanded — a genex literal probe, a non-expanded
// stamp trace, and staged nested File API queries — then reads each demand's
// output back. A project needing several is therefore reconfigured + re-lowered
// ONCE instead of once per demand; the hooks are orthogonal and none
// invalidates the warm try_compile/find_package cache. recovered is false when
// there were no demands, or the configure/read-backs yielded nothing (keep
// pass-1's result). sets/forwards default to the pass-1-abort recovery's copies
// (mutually exclusive with the stamp demand via the recoveredStampSets gate).
func runCoalescedWarmPass(ctx context.Context, a cli.Args, hostBuildDir string,
	literalSink *lower.LiteralProbeSink, stampSink, nestedSink map[string]string,
	recoveredStampSets []shadow.SetAssignment, recoveredStampForwards []shadow.ParentScopeForward,
	captureSink map[string]bool) warmRecovery {

	wr := warmRecovery{sets: recoveredStampSets, forwards: recoveredStampForwards}
	warm := a.TwoPassGenex && hostBuildDir != ""
	needGenex := warm && literalSink.Len() > 0
	needStamp := warm && len(stampSink) > 0 && len(recoveredStampSets) == 0
	needNested := warm && len(nestedSink) > 0
	needCapture := warm && len(captureSink) > 0
	var nestedRels []string
	if needNested {
		var staged int
		nestedRels, staged = stageNestedFileAPIQueries(hostBuildDir, nestedSink)
		needNested = staged > 0
	}
	if !needGenex && !needStamp && !needNested && !needCapture {
		return wr
	}

	opts, plainTrace, demands := warmConfigureOptions(a, hostBuildDir, literalSink, stampSink, nestedRels, captureSink, needGenex, needStamp, needNested, needCapture)
	fmt.Fprintf(os.Stderr, "convert-element-cmake: warm second configure for: %s.\n", strings.Join(demands, ", "))
	if _, cfgErr := cmakerun.Configure(ctx, opts); cfgErr != nil {
		// Non-fatal: keep pass-1's result for every demand (exactly as without
		// the two-pass feature). Loud, not silent.
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: warm second configure failed (%v); keeping pass-1 result.\n", cfgErr)
		return wr
	}

	if needGenex {
		if res, err := cmakerun.ReadLiteralProbe(hostBuildDir); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: reading literal-probe output failed (%v); genex literals stay unresolved.\n", err)
		} else if len(res) > 0 {
			wr.genexResolutions = res
			wr.recovered = true
		}
	}
	if needStamp && readStampSets(plainTrace, a.SourceRoot, &wr) {
		wr.recovered = true
	}
	if needNested {
		if nbs := harvestNestedBuilds(ctx, a, hostBuildDir, nestedRels, nestedSink); len(nbs) > 0 {
			wr.nestedBuilds = nbs
			wr.recovered = true
		}
	}
	if needCapture {
		if dead := readDeadCaptures(plainTrace, captureSink); len(dead) > 0 {
			wr.deadCaptureVars = dead
			wr.recovered = true
		}
	}
	return wr
}

// readDeadCaptures reads the warm pass's non-expanded trace and returns
// the capture-refusal variables the configure never reads — captures
// that exist only to silence console output (the dead-capture analysis;
// see lower/execute_process_dead_capture.go). Reads are only visible in
// the NON-expanded form: --trace-expand substitutes `${var}` away. Soft
// on read failure (nil — every refusal stands).
func readDeadCaptures(plainTrace string, captureSink map[string]bool) map[string]bool {
	raw, err := os.ReadFile(plainTrace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: reading non-expanded trace failed (%v); capture refusals stand.\n", err)
		return nil
	}
	reads := shadow.ExtractVariableReads(raw)
	dead := map[string]bool{}
	for v := range captureSink {
		if !reads[v] {
			dead[v] = true
		}
	}
	if len(dead) > 0 {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: %d capture variable(s) proven unread (silencing captures); re-lowering with their execute_process capture keywords cleared.\n", len(dead))
	}
	return dead
}

// warmConfigureOptions builds the union-of-hooks Configure options plus the
// human-readable demand list for the announce, per which demands fired.
func warmConfigureOptions(a cli.Args, hostBuildDir string, literalSink *lower.LiteralProbeSink,
	stampSink map[string]string, nestedRels []string, captureSink map[string]bool,
	needGenex, needStamp, needNested, needCapture bool) (cmakerun.Options, string, []string) {

	opts := cmakerun.Options{
		SourceRoot:         a.SourceRoot,
		BuildDir:           hostBuildDir,
		PrefixDir:          a.PrefixDir,
		ToolchainCMakeFile: a.ToolchainCMakeFile,
		BuildType:          a.BuildType,
		BuildTypes:         a.BuildTypes,
		Stdout:             os.Stderr,
		Stderr:             os.Stderr,
	}
	var plainTrace string
	var demands []string
	if needGenex {
		opts.LiteralProbes = literalSink.Requests()
		demands = append(demands, fmt.Sprintf("%d unresolved genex literal(s)", literalSink.Len()))
	}
	if needStamp {
		plainTrace = filepath.Join(hostBuildDir, "trace-plain.jsonl")
		opts.TracePath = plainTrace
		opts.TraceNonExpanded = true
		demands = append(demands, fmt.Sprintf("%d VCS-stamp var(s)", len(stampSink)))
	}
	if needNested {
		demands = append(demands, fmt.Sprintf("%d nested cmake build(s)", len(nestedRels)))
	}
	if needCapture {
		// Same instrument as the stamp demand: the non-expanded trace.
		if plainTrace == "" {
			plainTrace = filepath.Join(hostBuildDir, "trace-plain.jsonl")
			opts.TracePath = plainTrace
			opts.TraceNonExpanded = true
		}
		demands = append(demands, fmt.Sprintf("%d capture-refused execute_process var(s) (dead-capture analysis)", len(captureSink)))
	}
	return opts, plainTrace, demands
}

// readStampSets reads the non-expanded trace and, when it carries set()-copies
// or parent-scope forwards, stores them on wr (overriding the pass-1-abort
// defaults). Returns whether anything was recovered.
func readStampSets(plainTrace, sourceRoot string, wr *warmRecovery) bool {
	raw, err := os.ReadFile(plainTrace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: reading non-expanded trace failed (%v); keeping direct stamp vars only.\n", err)
		return false
	}
	sets := shadow.ExtractSetAssignments(raw, sourceRoot)
	forwards := shadow.ExtractParentScopeForwards(raw, sourceRoot)
	if len(sets) == 0 && len(forwards) == 0 {
		return false
	}
	wr.sets = sets
	wr.forwards = forwards
	return true
}

// recoverPass1CaptureAbort attempts to rescue a pass-1 ToIR abort caused
// by capture-bearing execute_process refusals: a warm non-expanded-trace
// configure proves which captured variables are never read (silencing
// captures), and a re-lower with those capture keywords cleared lifts
// the calls instead of aborting. deadOut receives the proven-dead set
// (read by the runToIR closure). ok=false when the configure fails, no
// capture is provably dead, or the re-lower still errors — the caller
// surfaces the original error.
func recoverPass1CaptureAbort(ctx context.Context, a cli.Args, hostBuildDir string, captureSink map[string]bool, deadOut *map[string]bool, relower func() (*ir.Package, error)) (*ir.Package, map[string]bool, bool) {
	tracePath, err := warmPlainTraceConfigure(ctx, a, hostBuildDir)
	if err != nil {
		return nil, nil, false
	}
	dead := readDeadCaptures(tracePath, captureSink)
	if len(dead) == 0 {
		return nil, nil, false
	}
	*deadOut = dead
	pkg, err := relower()
	if err != nil {
		return nil, nil, false
	}
	return pkg, dead, true
}

// warmPlainTraceConfigure runs the warm NON-expanded-trace reconfigure
// every plain-trace consumer shares (the stamp set-copy rescue, the
// capture-abort rescue, the coalesced warm pass's stamp/capture
// demands) and returns the trace path. One home for the option
// literal: a new required warm-configure knob lands here once.
func warmPlainTraceConfigure(ctx context.Context, a cli.Args, hostBuildDir string) (string, error) {
	tracePath := filepath.Join(hostBuildDir, "trace-plain.jsonl")
	_, err := cmakerun.Configure(ctx, cmakerun.Options{
		SourceRoot:         a.SourceRoot,
		BuildDir:           hostBuildDir,
		PrefixDir:          a.PrefixDir,
		ToolchainCMakeFile: a.ToolchainCMakeFile,
		BuildType:          a.BuildType,
		BuildTypes:         a.BuildTypes,
		TracePath:          tracePath,
		TraceNonExpanded:   true,
		Stdout:             os.Stderr,
		Stderr:             os.Stderr,
	})
	return tracePath, err
}
