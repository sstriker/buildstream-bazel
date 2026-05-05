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
	// Round-2 dispatch only activates when the trace-driven CLI is
	// configured AND the element compiles native code (the
	// converter recovers cc rules from execve events; an element
	// that just stages files produces an empty BUILD.bazel.out
	// placeholder, which is exactly the round-2 boot-phase signal).
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
