package main

import (
	"strings"
	"testing"
)

// TestCmakeSrckeyPatterns_ContentInclusionTruthTable covers the
// per-glob content-included vs path-only verdict for the file
// types kind:cmake's round-2 fallback cares about. Keeps the
// pattern set's intent locked into tests so a future edit that
// accidentally drops a glob (e.g. .h family) trips the test
// rather than silently weakening srckey coverage.
func TestCmakeSrckeyPatterns_ContentInclusionTruthTable(t *testing.T) {
	p := cmakeSrckeyPatterns()

	cases := []struct {
		path string
		want bool
	}{
		// Top-level CMakeLists + nested.
		{"CMakeLists.txt", true},
		{"sub/CMakeLists.txt", true},
		{"src/sub/sub2/CMakeLists.txt", true},
		// cmake module includes (top-level and nested).
		{"cmake/Helpers.cmake", true},
		{"cmake/Versions.cmake", true},
		{"src/sub/Local.cmake", true},
		// .cmake.in templates.
		{"cmake/ProjectConfig.cmake.in", true},
		{"cmake/Versions.cmake.in", true},
		// Header family — content shifts include resolution.
		{"include/foo.h", true},
		{"include/foo.hpp", true},
		{"include/foo.hxx", true},
		{"include/foo.hh", true},
		{"src/internal/bar.h", true},
		// CMake presets — alternative configure entry points.
		{"CMakePresets.json", true},
		{"CMakeUserPresets.json", true},

		// .h.in — path-only by default. The configure_file
		// lift makes them Bazel-srcs covered; elements
		// without the lift add `include **/*.h.in` per-element
		// (the audit flags them as undercoverage drift since
		// cmake's RERUN_CMAKE oracle reports them as reads).
		{"src/config.h.in", false},
		{"include/cap.h.in", false},

		// Source files: content-only (no rule matches → not
		// content-included). Edits invalidate via Bazel's
		// action cache, not via srckey.
		{"src/foo.c", false},
		{"src/foo.cc", false},
		{"src/foo.cpp", false},
		{"src/foo.cxx", false},
		{"src/asm/boot.s", false},
		// Build outputs / unrelated.
		{"build/foo.o", false},
		{"README.md", false},
		{"docs/usage.adoc", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			got := p.Match(c.path)
			if got != c.want {
				t.Errorf("Match(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestCmakeSrckeyPatterns_DistinctFromAutotoolsAndMake guards
// against accidental cross-contamination: kind:cmake's pattern
// set should not include autotools-only entries (configure.ac,
// *.am, *.m4) and should not include kind:make's broad
// "Makefile" pattern. Catches a copy-paste regression that
// would muddle the per-kind narrowing decisions.
func TestCmakeSrckeyPatterns_DistinctFromAutotoolsAndMake(t *testing.T) {
	p := cmakeSrckeyPatterns()
	for _, foreign := range []string{
		"configure.ac",
		"Makefile.am",
		"acinclude.m4",
		"Makefile",
		"src/Makefile",
		"Build.PL",
		"Makefile.PL",
	} {
		if p.Match(foreign) {
			t.Errorf("cmakeSrckeyPatterns unexpectedly matches %q (belongs to a different kind)", foreign)
		}
	}
}

// TestWrapCmakePipelineCmds_ShellShape locks in the load-bearing
// pieces of the build-tracer wrapper: the exec-root anchored
// tool reference, the three --normalize-prefix substitutions
// matching the autotools precedent, and the inline `sh -c '...'`
// frame around the caller-supplied cmds. The wrapper's exact
// formatting can drift in cosmetic ways; the assertions below
// pin the contract pieces only.
func TestWrapCmakePipelineCmds_ShellShape(t *testing.T) {
	// The canonical shape uses BUILD_ROOT/INSTALL_ROOT/SRC_DIR
	// — the same variable names the surrounding install genrule
	// binds in its prelude. --normalize-prefix below rewrites
	// those exact prefixes into stable placeholders, so the
	// test cmds use the same names for accurate coverage.
	cmds := `cmake -B "$$BUILD_ROOT" -G Ninja -S "$$SRC_DIR" -DCMAKE_INSTALL_PREFIX="$$INSTALL_ROOT"
cmake --build "$$BUILD_ROOT" --parallel 1
cmake --install "$$BUILD_ROOT" --prefix "$$INSTALL_ROOT"`
	got := wrapCmakePipelineCmds(cmds)

	for _, want := range []string{
		`"$$EXEC_ROOT/$(location //tools:build-tracer)"`,
		`--normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT"`,
		`--normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT"`,
		`--normalize-prefix="$${DEP_PREFIX:-/__unset_dep_prefix__}=/DEP_PREFIX"`,
		`--out="$$CMAKE_TRACE"`,
		`-- sh -c '`,
		`cmake -B "$$BUILD_ROOT" -G Ninja -S "$$SRC_DIR" -DCMAKE_INSTALL_PREFIX="$$INSTALL_ROOT"`,
		`cmake --build "$$BUILD_ROOT" --parallel 1`,
		`cmake --install "$$BUILD_ROOT" --prefix "$$INSTALL_ROOT"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapCmakePipelineCmds output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestWrapCmakePipelineCmds_DistinctFromAutotools asserts the
// kind:cmake wrapper uses CMAKE_TRACE (not AUTOTOOLS_TRACE) for
// the trace tmpfile. An orchestrator pipeline that has both a
// kind:cmake and a kind:autotools element shouldn't have the
// two tracers stomp on each other's tmpfile.
func TestWrapCmakePipelineCmds_DistinctFromAutotools(t *testing.T) {
	got := wrapCmakePipelineCmds("noop")
	if !strings.Contains(got, "CMAKE_TRACE") {
		t.Errorf("wrapCmakePipelineCmds should reference CMAKE_TRACE; got:\n%s", got)
	}
	if strings.Contains(got, "AUTOTOOLS_TRACE") {
		t.Errorf("wrapCmakePipelineCmds should not reference AUTOTOOLS_TRACE; got:\n%s", got)
	}
}
