package main

func init() {
	// kind:makemaker is BuildStream's `makemaker` plugin lowered
	// onto the pipelineHandler shape. The plugin builds Perl
	// modules that ship a Makefile.PL via Perl's MakeMaker:
	//
	//	configure-commands: ["perl Makefile.PL PREFIX=%{prefix} INSTALLDIRS=vendor"]
	//	build-commands:     ["make"]
	//	install-commands:   ["make DESTDIR=\"%{install-root}\" install"]
	//
	// Defaults mirror BuildStream's plugin
	// (buildstream-plugins/src/buildstream_plugins/elements/makemaker.py).
	// FDSDK uses kind:makemaker for Perl modules in the bootstrap
	// path (perl-build, perl modules under bootstrap/base-sdk/).
	// Surfaced empirically by aom.bst's subgraph at
	// components/perl-build.
	//
	// kind:makemaker opts into the trace-driven round-2 path.
	// Perl XS modules go through xsubpp → cc → ld, which the
	// build-tracer captures the same way it does for kind:make /
	// kind:autotools. See makemakerSrckeyPatterns below.
	registerHandler(pipelineHandler{
		kindName: "makemaker",
		defaults: pipelineDefaults{
			Configure: []string{
				`perl Makefile.PL PREFIX=%{prefix} INSTALLDIRS=vendor`,
			},
			Build: []string{
				`make`,
			},
			Install: []string{
				`make DESTDIR="%{install-root}" install`,
			},
		},
		traceDrivenSrckeyPatterns: makemakerSrckeyPatterns(),
	})
}

// makemakerSrckeyPatterns is the per-kind narrowing for
// kind:makemaker's build-graph srckey. Same content-vs-name
// decision as autotoolsSrckeyPatterns: files that determine
// WHICH cc commands fire are content-included; files that only
// affect compile OUTPUT bytes contribute by path only.
//
// Content-included families:
//
//   - Makefile.PL — the author-written driver that `perl
//     Makefile.PL` reads to generate the Makefile. Its content
//     drives every recipe in the generated Makefile.
//   - **/Makefile — generated, but dependees may ship a
//     pre-generated one alongside Makefile.PL; covered for
//     safety.
//   - **/*.xs — Perl XS files. xsubpp translates these to *.c
//     before cc runs; the *.xs content (not the generated *.c)
//     determines which cc commands fire.
//   - **/*.h / *.hpp / *.hxx — header content can conditionally
//     affect compile flags. Same conservative decision the
//     autotools handler makes.
//
// Excluded by default (path-only): *.c (often generated from
// *.xs), *.cpp, *.pm (pure Perl, doesn't drive cc), *.o.
func makemakerSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{
		Rules: []patternRule{
			{Include: true, Pattern: "Makefile.PL"},
			{Include: true, Pattern: "**/Makefile.PL"},
			{Include: true, Pattern: "Makefile"},
			{Include: true, Pattern: "**/Makefile"},
			{Include: true, Pattern: "**/*.xs"},
			{Include: true, Pattern: "**/*.h"},
			{Include: true, Pattern: "**/*.hpp"},
			{Include: true, Pattern: "**/*.hxx"},
		},
	}
}
