package main

func init() {
	// kind:manual is the most general pipeline kind: no defaults.
	// The .bst's config: block fully specifies which phase commands
	// run. Sibling kinds (kind:make / kind:autotools / ...) layer
	// default phase commands on the same pipelineHandler shape.
	//
	// kind:manual opts into the trace-driven round-2 path with
	// the conservative content-everything srckey rule set
	// (manualScriptSrckeyPatterns). The .bst's commands could be
	// anything — there's no kind-level signal for "Makefile.PL is
	// the driver" that kind:makemaker has. So every file's content
	// contributes to srckey, which means any source change
	// invalidates the cache. That's the loose-bound default;
	// per-element narrowing via the existing read-paths.txt sibling
	// (or future per-element traceDrivenSrckeyPatterns override)
	// can tighten this for elements with predictable shapes.
	//
	// Round-2 dispatch activates whenever the trace-driven CLI
	// is configured (traceConfig.convertBin set +
	// round2Enabled true) AND the kind opted in via
	// traceDrivenSrckeyPatterns — see pipelineHandler.shouldUseRound2.
	// Whether the converter recovers cc rules vs emits the
	// boot-phase placeholder is a separate question answered at
	// converter-action time: a manual element whose commands
	// don't run cc / ar / ld produces a trace with no compile
	// events, the converter sees nothing to recover, and
	// BUILD.bazel.out comes out empty. Project B's coarse install
	// genrule remains the buildable target in that case (same as
	// the AC-miss boot phase). Net: opting kind:manual in is
	// safe for non-native elements; they just stay coarse.
	registerHandler(pipelineHandler{
		kindName:                  "manual",
		traceDrivenSrckeyPatterns: manualScriptSrckeyPatterns(),
	})
}

// manualScriptSrckeyPatterns is shared between kind:manual and
// kind:script — both lack any kind-level convention about which
// files drive build commands, so the patterns set is empty
// (matchesSrckeyPatterns treats nil/empty as "every file is
// content-included"). Any per-element narrowing comes from the
// element's read-paths.txt sibling, not from a kind default.
func manualScriptSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{}
}
