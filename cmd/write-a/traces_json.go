package main

// Trace-driven kind gating + helpers.
//
// Historically this file emitted tools/traces.json: one record per
// element whose kind participates in the trace-driven round-2 path.
// The data file was consumed by a `traces` module extension
// (rules/traces.bzl) whose `_trace_repo` repository rule queried
// the REAPI ActionCache at Bazel LOAD time. That whole pipeline
// retired when the AC lookup moved to action time via the new
// trace_load rule — the per-element trace_load target carries its
// srckey + platform as string attrs directly, so no separate JSON
// manifest is needed.
//
// What survives here:
//
//   - traceDrivenSrckeyPatternsForKind: the per-kind classifier
//     (autotools special case, pipelineHandler-shaped kinds opted
//     in via traceDrivenSrckeyPatterns, kind:cmake / kind:meson
//     under their round-2 fallback flags).

// traceDrivenSrckeyPatternsForKind returns the per-kind srckey
// pattern set when the kind is opted into trace-driven round-2,
// or nil otherwise. Source of truth for "is this kind in the
// trace-driven set."
//
// kind:autotools is special-cased here because its dispatch lives
// in autotoolsHandler (handler_autotools_native.go) rather than
// going through pipelineHandler's traceDrivenSrckeyPatterns
// field. The autotools + pipeline arms only return non-nil when
// traceConfig.round2Enabled is set (with convertBin staged):
// without round-2 active, those kinds don't reference the
// trace_load target in their rendered BUILDs.
// kind:cmake / kind:meson are special-cased too: not pipeline
// handlers, but join the trace-driven path via their round-2
// fallback shapes when *Config.round2FallbackEnabled is set.
func traceDrivenSrckeyPatternsForKind(kind string) *readPathsPatterns {
	autotoolsRound2 := traceConfig.convertBin != "" && traceConfig.round2Enabled
	if kind == "autotools" {
		if !autotoolsRound2 {
			return nil
		}
		return autotoolsSrckeyPatterns()
	}
	if kind == "cmake" && cmakeConfig.round2FallbackEnabled {
		return cmakeSrckeyPatterns()
	}
	if kind == "meson" && mesonConfig.round2FallbackEnabled {
		return mesonSrckeyPatterns()
	}
	h, ok := handlers[kind]
	if !ok {
		return nil
	}
	ph, ok := h.(pipelineHandler)
	if !ok {
		return nil
	}
	if !autotoolsRound2 {
		// Pipeline-kind dispatch through pipelineHandler.
		// shouldUseRound2() also requires this gate; without
		// it, the trace_load rule renders for elements whose
		// BUILD won't reference it.
		return nil
	}
	return ph.traceDrivenSrckeyPatterns
}
