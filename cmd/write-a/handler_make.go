package main

func init() {
	// kind:make is BuildStream's `make` plugin lowered onto the
	// pipelineHandler shape. The plugin's defaults (per
	// buildstream/src/buildstream/plugins/elements/make.py) are:
	//
	//	build-commands:   ["make %{make-args}"]
	//	install-commands: ["make -j1 %{make-install-args}"]
	//
	// with the variable defaults:
	//
	//	make-args         = ""
	//	make-install-args = `DESTDIR="%{install-root}" install`
	//
	// The variable resolver (variables.go) expands these at codegen
	// time: %{make-install-args} is fully replaced with the
	// substituted RHS, then the runtime sentinel %{install-root}
	// becomes $$INSTALL_ROOT during command rendering. End result is
	// the same `make -j1 DESTDIR="$$INSTALL_ROOT" install` an
	// FDSDK-style .bst would emit, and an element overriding
	// %{make-install-args} (or %{make-args}) gets per-element
	// behavior without re-stating the surrounding `make ...` shape.
	//
	// kind:make also opts into the trace-driven round-2 path via
	// makeSrckeyPatterns (below). When the operator passes the
	// trace-driven binaries to write-a (--convert-element-trace
	// + --build-tracer-bin + --trace-publish-bin + --trace-lookup-bin)
	// AND doesn't pass --trace-round1, kind:make elements
	// render with the same round-2 architecture as kind:autotools:
	// project A hosts a per-element converter genrule consuming
	// @trace_<elem>//:trace; project B hosts the coarse install
	// genrule wrapped in build-tracer with an inline trace-publish
	// step. See docs/design/autotools-round2-rendezvous.md for
	// the recipe (kind-agnostic; the doc title is autotools-named
	// for historical reasons).
	registerHandler(pipelineHandler{
		kindName: "make",
		defaultVars: map[string]string{
			"make-args":         "",
			"make-install-args": `DESTDIR="%{install-root}" install`,
		},
		defaults: pipelineDefaults{
			Build: []string{
				`make %{make-args}`,
			},
			Install: []string{
				`make -j1 %{make-install-args}`,
			},
		},
		traceDrivenSrckeyPatterns: makeSrckeyPatterns(),
	})
}

// makeSrckeyPatterns is the per-kind narrowing rule set for
// kind:make's build-graph srckey. Same shape decision as
// autotoolsSrckeyPatterns (see handler_autotools_native.go's
// rationale): files that determine the BUILD COMMANDS make emits
// are content-included (their bytes contribute to srckey); files
// that only affect compiler OUTPUT bytes contribute by path only.
//
// Content-included families:
//
//   - Makefile + nested Makefiles — the source of truth for
//     make's recipes. Content matters: editing a recipe changes
//     which compile commands fire.
//   - **/*.h / **/*.hpp / **/*.hxx — header content can
//     conditionally affect compile flags emitted by recursive
//     Makefiles via per-file CFLAGS overrides. Same conservative
//     decision the autotools handler makes for the same reason.
//
// Everything else (.c / .cc / .cpp / .S / .s / build outputs) is
// name-only — adding/removing a source file invalidates srckey
// (a wildcard rule could pick it up); editing an existing source
// file's content does not. That's the property that lets
// edit-and-rebuild cycles cache-hit on srckey while genuine
// graph edits invalidate cleanly.
func makeSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{
		Rules: []patternRule{
			{Include: true, Pattern: "Makefile"},
			{Include: true, Pattern: "**/Makefile"},
			{Include: true, Pattern: "**/*.h"},
			{Include: true, Pattern: "**/*.hpp"},
			{Include: true, Pattern: "**/*.hxx"},
		},
	}
}
