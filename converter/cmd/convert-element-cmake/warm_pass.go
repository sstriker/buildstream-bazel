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
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// warmRecovery is what the coalesced warm second configure recovered, threaded
// into the single re-lower.
type warmRecovery struct {
	genexResolutions map[string]cmakerun.LiteralResolution
	sets             []shadow.SetAssignment
	forwards         []shadow.ParentScopeForward
	nestedBuilds     []lower.NestedBuildInput
	recovered        bool
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
	recoveredStampSets []shadow.SetAssignment, recoveredStampForwards []shadow.ParentScopeForward) warmRecovery {

	wr := warmRecovery{sets: recoveredStampSets, forwards: recoveredStampForwards}
	warm := a.TwoPassGenex && hostBuildDir != ""
	needGenex := warm && literalSink.Len() > 0
	needStamp := warm && len(stampSink) > 0 && len(recoveredStampSets) == 0
	needNested := warm && len(nestedSink) > 0
	var nestedRels []string
	if needNested {
		var staged int
		nestedRels, staged = stageNestedFileAPIQueries(hostBuildDir, nestedSink)
		needNested = staged > 0
	}
	if !needGenex && !needStamp && !needNested {
		return wr
	}

	opts, plainTrace, demands := warmConfigureOptions(a, hostBuildDir, literalSink, stampSink, nestedRels, needGenex, needStamp, needNested)
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
	return wr
}

// warmConfigureOptions builds the union-of-hooks Configure options plus the
// human-readable demand list for the announce, per which demands fired.
func warmConfigureOptions(a cli.Args, hostBuildDir string, literalSink *lower.LiteralProbeSink,
	stampSink map[string]string, nestedRels []string, needGenex, needStamp, needNested bool) (cmakerun.Options, string, []string) {

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
