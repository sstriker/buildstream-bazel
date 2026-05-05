package main

func init() {
	// kind:modulebuild is BuildStream's `modulebuild` plugin
	// lowered onto the pipelineHandler shape. Perl modules
	// shipping a Build.PL (Module::Build) instead of a Makefile.PL
	// (ExtUtils::MakeMaker) take this path:
	//
	//	configure-commands: ["perl Build.PL --prefix=%{prefix} --installdirs=vendor"]
	//	build-commands:     ["./Build"]
	//	install-commands:   ["./Build install --destdir \"%{install-root}\""]
	//
	// Defaults mirror BuildStream's plugin
	// (buildstream-plugins/src/buildstream_plugins/elements/modulebuild.py).
	// Surfaced empirically by aom.bst's subgraph at components/po4a.
	//
	// kind:modulebuild opts into the trace-driven round-2 path.
	// XS modules under Module::Build still go through cc/ld;
	// the build-tracer captures the events whether they come
	// from a Makefile or a Perl Build script. See
	// modulebuildSrckeyPatterns below.
	registerHandler(pipelineHandler{
		kindName: "modulebuild",
		defaults: pipelineDefaults{
			Configure: []string{
				`perl Build.PL --prefix=%{prefix} --installdirs=vendor`,
			},
			Build: []string{
				`./Build`,
			},
			Install: []string{
				`./Build install --destdir "%{install-root}"`,
			},
		},
		traceDrivenSrckeyPatterns: modulebuildSrckeyPatterns(),
	})
}

// modulebuildSrckeyPatterns mirrors makemakerSrckeyPatterns but
// for Module::Build's Build.PL driver. `perl Build.PL` reads
// Build.PL to generate the `Build` script; the script's content
// drives compile commands the same way a Makefile would. *.xs
// files still drive cc invocations through Module::Build's
// xsubpp integration.
func modulebuildSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{
		Rules: []patternRule{
			{Include: true, Pattern: "Build.PL"},
			{Include: true, Pattern: "**/Build.PL"},
			{Include: true, Pattern: "**/*.xs"},
			{Include: true, Pattern: "**/*.h"},
			{Include: true, Pattern: "**/*.hpp"},
			{Include: true, Pattern: "**/*.hxx"},
		},
	}
}
