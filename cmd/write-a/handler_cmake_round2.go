package main

import "fmt"

// handler_cmake_round2.go: kind-specific scaffolding for the
// kind:cmake round-2 fallback.
//
// Phase A (PR #95) emits unsupported-execute-process Tier-1
// failures for execute_process calls the classifier can't lift
// natively. Phase B replaces the exclusion with a round-2
// coarse genrule fallback (build-tracer-wrapped cmake configure
// + ninja + install) so unliftable elements still produce a
// downstream artifact.
//
// This file holds the kind-specific scaffolding pieces (the
// srckey pattern set + the build-tracer wrapping helper) as
// pure functions with no call sites yet. Subsequent steps wire
// them into write-a's render path. See
// docs/design/cmake-execute-process-round2-fallback.md for the
// architectural shape and staged plan.
//
// The kind-agnostic round-2 helpers
// (renderTraceDrivenRound2A, pipelineTraceExtensionRound2 in
// handler_pipeline_round2.go) are reused as-is once kind:cmake
// joins; only the bits below are kind-specific.

// cmakeSrckeyPatterns returns the file-glob set that gates
// srckey content-inclusion for kind:cmake round-2.
//
// Content-included (rule.Include == true): paths whose **byte
// content** changes the trace cmake configure + ninja + install
// would record. Path-only (no rule, default-deny): everything
// else — adding/removing a path invalidates srckey via the
// universe, but editing an existing file's content does not.
//
// Per-pattern rationale:
//
//   - CMakeLists.txt + nested CMakeLists.txt — the canonical
//     declarative driver. Every directive (add_library,
//     target_compile_options, add_subdirectory, etc.) reshapes
//     the build graph cmake records.
//   - *.cmake (top-level + nested) — cmake module includes
//     (find_package logic, helper functions, generator-expr
//     macros). Same shape as CMakeLists.txt: edits change
//     command lines.
//   - *.cmake.in — templates substituted by configure_file at
//     cmake configure time. Output is consumed in cmake's own
//     reconfigure machinery (ProjectConfig.cmake.in etc.); the
//     resulting .cmake file feeds the build graph the same
//     way a hand-written .cmake would.
//   - **/*.h family + **/*.hpp / **/*.hxx / **/*.hh — header
//     edits shift include resolution and #include-driven
//     conditionals, which surface in compile commands the
//     trace records (added/removed -I dirs, distinct
//     dependency edges in ninja's deps DB).
//   - CMakePresets.json / CMakeUserPresets.json — alternative
//     configure entry points; their content reshapes the
//     configure command without going through CMakeLists.txt.
//
// **`.h.in` default is path-only.** cmake itself reads `.h.in`
// at configure time (configure_file substitutes them), so the
// `RERUN_CMAKE` oracle always flags them. The configure_file
// lift in PR #94 makes `.h.in` Bazel-srcs covered, removing
// the need for srckey content-inclusion. The kind default
// reflects that steady state: `.h.in` is path-only by default;
// elements without the lift staged surface undercoverage drift
// in `audit-narrowing` (the cmake oracle reports `.h.in`, the
// patterns don't cover it). Operators react by either staging
// the configure_file lift OR adding a per-element
// `include **/*.h.in` override to read-paths.txt. See
// `docs/design/narrowing-audit.md` "*.h.in and the
// configure_file lift" for the trade-off.
//
// Path-only (no rule) for: *.c / *.cc / *.cpp / *.cxx /
// *.C / *.s / *.S — compile sources. The trace records the
// compile command (`gcc -c foo.c -o foo.o`) regardless of
// foo.c's bytes; edits invalidate via Bazel's action cache,
// not via srckey.
//
// Mirrors makeSrckeyPatterns / autotoolsSrckeyPatterns shape
// — same Include semantics, same default-deny behaviour for
// unmatched paths.
func cmakeSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{
		Rules: []patternRule{
			{Include: true, Pattern: "CMakeLists.txt"},
			{Include: true, Pattern: "**/CMakeLists.txt"},
			{Include: true, Pattern: "*.cmake"},
			{Include: true, Pattern: "**/*.cmake"},
			{Include: true, Pattern: "*.cmake.in"},
			{Include: true, Pattern: "**/*.cmake.in"},
			{Include: true, Pattern: "**/*.h"},
			{Include: true, Pattern: "**/*.hpp"},
			{Include: true, Pattern: "**/*.hxx"},
			{Include: true, Pattern: "**/*.hh"},
			// CMakePresets / kits — build-driver inputs that
			// reshape the configure command without going
			// through CMakeLists.txt.
			{Include: true, Pattern: "CMakePresets.json"},
			{Include: true, Pattern: "CMakeUserPresets.json"},
		},
	}
}

// wrapCmakePipelineCmds wraps a cmake configure + build +
// install sequence under build-tracer so the trace artifact
// captures every execve under the build sandbox. Mirrors
// wrapAutotoolsPipelineCmds (handler_autotools_native.go) — same
// --normalize-prefix substitutions, same single-line-per-step
// shell shape, same trace tmpfile pattern.
//
// The cmds argument is the resolved configure / build / install
// shell snippet (already variable-substituted by the caller).
// For kind:cmake round-2 the canonical shape is:
//
//	cmake -B "$$BUILD_DIR" -G Ninja -S "$$SRC_DIR" \
//	    -DCMAKE_INSTALL_PREFIX="$$DESTDIR" [...]
//	cmake --build "$$BUILD_DIR" --parallel 1
//	cmake --install "$$BUILD_DIR" --prefix "$$DESTDIR"
//
// --parallel 1 mirrors `make -j1` in autotools round-2: serial
// execution keeps the trace's process-spawn order stable so
// canonicalization byte-equality holds across recordings on
// different machines.
//
// The tracer-out path lives under $$CMAKE_TRACE so the post-
// pipeline trace-publish step can find it; the tmpfile is
// machine-local and the canonical bytes are written to a
// genrule output. CMAKE_TRACE specifically (not the autotools
// AUTOTOOLS_TRACE) so an element with both kinds in its
// orchestrator pipeline doesn't have the two tracers stomp on
// each other's tmpfile.
//
// Path note: $(location //tools:build-tracer) resolves to an
// exec-root-relative path; the prelude already cd's into
// $$BUILD_ROOT before this runs, so we anchor explicitly to
// $$EXEC_ROOT to find the staged binary.
//
// Step 1 lands this as a pure function with no call sites.
// Step 3 wires it into the per-element round-2 install genrule
// emission.
func wrapCmakePipelineCmds(cmds string) string {
	return fmt.Sprintf(`        # Build-tracer wraps the entire cmake configure / build /
        # install pipeline. The trace artifact captures every
        # execve under the build sandbox; pass-3's trace-publish
        # canonicalizes it and lands an AC entry keyed by
        # SyntheticActionDigest(srckey).
        #
        # --normalize-prefix substitutions neutralize action-time
        # mktemp paths (INSTALL_ROOT, BUILD_ROOT, DEP_PREFIX). Their
        # bytes vary across bazel invocations even when the build
        # is otherwise identical, so without normalization the
        # canonical trace would still drift run-to-run. The
        # placeholder names (/INSTALL_ROOT, etc.) are stable
        # across machines and human-readable. DEP_PREFIX is only
        # set when the element has cmake deps — using
        # ${DEP_PREFIX:-} keeps the flag harmless when unset
        # (substitutes empty-string, which trivially matches
        # nothing).
        export CMAKE_TRACE="$$(mktemp)"
        "$$EXEC_ROOT/$(location //tools:build-tracer)" \
            --normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT" \
            --normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT" \
            --normalize-prefix="$${DEP_PREFIX:-/__unset_dep_prefix__}=/DEP_PREFIX" \
            --out="$$CMAKE_TRACE" -- sh -c '
%s
'
`, cmds)
}
