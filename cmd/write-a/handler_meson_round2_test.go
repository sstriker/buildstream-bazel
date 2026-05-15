package main

import (
	"strings"
	"testing"
)

// TestMesonSrckeyPatterns_ContentInclusionTruthTable covers the
// per-glob content-included vs path-only verdict for the file
// types kind:meson's round-2 fallback cares about. Mirrors the
// kind:cmake / kind:autotools shape — locks the pattern set's
// intent into tests so a future edit that accidentally drops a
// glob trips the test rather than silently weakening srckey
// coverage.
func TestMesonSrckeyPatterns_ContentInclusionTruthTable(t *testing.T) {
	p := mesonSrckeyPatterns()

	cases := []struct {
		path string
		want bool
	}{
		// Top-level meson.build + nested.
		{"meson.build", true},
		{"sub/meson.build", true},
		{"src/sub/sub2/meson.build", true},
		// meson option declaration files (both legacy and modern names).
		{"meson_options.txt", true},
		{"sub/meson_options.txt", true},
		{"meson.options", true},
		{"src/meson.options", true},
		// Header family — content shifts include resolution.
		{"include/foo.h", true},
		{"include/foo.hpp", true},
		{"include/foo.hxx", true},
		{"include/foo.hh", true},
		{"src/internal/bar.h", true},

		// Source files: content-only (no rule matches → not
		// content-included). Edits invalidate via Bazel's action
		// cache, not via srckey.
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

// TestMesonSrckeyPatterns_DistinctFromCmakeAndAutotools guards
// against accidental cross-contamination: kind:meson's pattern
// set shouldn't include cmake-only entries (CMakeLists.txt,
// *.cmake) or autotools-only entries (configure.ac, *.am, *.m4).
// Catches a copy-paste regression that would muddle the per-kind
// narrowing decisions.
func TestMesonSrckeyPatterns_DistinctFromCmakeAndAutotools(t *testing.T) {
	p := mesonSrckeyPatterns()
	for _, foreign := range []string{
		"CMakeLists.txt",
		"src/CMakeLists.txt",
		"cmake/Helpers.cmake",
		"configure.ac",
		"Makefile.am",
		"acinclude.m4",
		"Makefile",
		"Build.PL",
		"Makefile.PL",
	} {
		if p.Match(foreign) {
			t.Errorf("mesonSrckeyPatterns unexpectedly matches %q (belongs to a different kind)", foreign)
		}
	}
}

// TestWrapMesonPipelineCmds_ShellShape locks in the load-bearing
// pieces of the build-tracer wrapper: the exec-root-anchored tool
// reference, the three --normalize-prefix substitutions matching
// the cmake/autotools precedent, the kind-specific MESON_TRACE
// tmpfile binding, and the inline `sh -c '...'` frame around the
// caller-supplied cmds. The wrapper's exact formatting can drift
// in cosmetic ways; the assertions below pin the contract pieces
// only.
func TestWrapMesonPipelineCmds_ShellShape(t *testing.T) {
	// The canonical shape uses BUILD_ROOT / INSTALL_ROOT / SRC_DIR —
	// the same variable names the surrounding install genrule binds
	// in its prelude. --normalize-prefix below rewrites those exact
	// prefixes into stable placeholders, so the test cmds use the
	// same names for accurate coverage.
	cmds := `meson setup "$$BUILD_ROOT" "$$SRC_DIR" --prefix=/ --libdir=lib
ninja -C "$$BUILD_ROOT"
DESTDIR="$$INSTALL_ROOT" meson install -C "$$BUILD_ROOT"`
	got := wrapMesonPipelineCmds(cmds)

	for _, want := range []string{
		`"$$EXEC_ROOT/$(location //tools:build-tracer)"`,
		`--normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT"`,
		`--normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT"`,
		`--normalize-prefix="$${DEP_PREFIX:-/__unset_dep_prefix__}=/DEP_PREFIX"`,
		`--out="$$MESON_TRACE"`,
		`-- sh -c '`,
		`meson setup "$$BUILD_ROOT" "$$SRC_DIR" --prefix=/ --libdir=lib`,
		`ninja -C "$$BUILD_ROOT"`,
		`DESTDIR="$$INSTALL_ROOT" meson install -C "$$BUILD_ROOT"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapMesonPipelineCmds output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestWrapMesonPipelineCmds_DistinctTraceVar asserts the kind:meson
// wrapper uses MESON_TRACE (not CMAKE_TRACE / AUTOTOOLS_TRACE) for
// the trace tmpfile. An orchestrator pipeline that has kind:meson
// alongside kind:cmake or kind:autotools elements shouldn't have
// the tracers stomp on each other's tmpfile.
func TestWrapMesonPipelineCmds_DistinctTraceVar(t *testing.T) {
	got := wrapMesonPipelineCmds("noop")
	if !strings.Contains(got, "MESON_TRACE") {
		t.Errorf("wrapMesonPipelineCmds should reference MESON_TRACE; got:\n%s", got)
	}
	for _, foreign := range []string{"CMAKE_TRACE", "AUTOTOOLS_TRACE"} {
		if strings.Contains(got, foreign) {
			t.Errorf("wrapMesonPipelineCmds should not reference %s; got:\n%s", foreign, got)
		}
	}
}

// TestMesonRound2InstallBuild_RenderShape covers the single-
// platform install genrule's load-bearing markers: the
// elem-prefixed genrule name, install_tree.tar / trace.log outs,
// build-tracer + trace-publish tools, the meson configure/build/
// install pipeline commands, and the trace-publish guard against
// empty CAS_GRPC_ADDR. The full BUILD body is too long to assert
// verbatim; this contract-test mirrors the kind:cmake equivalent.
func TestMesonRound2InstallBuild_RenderShape(t *testing.T) {
	elem := &element{Name: "demo"}
	got := mesonRound2InstallBuild(elem, tracePlatform{})

	for _, want := range []string{
		`name = "demo_install"`,
		`"install_tree.tar"`,
		`"trace.log"`,
		`"//tools:build-tracer"`,
		`"//tools:trace-publish"`,
		`meson setup "$$BUILD_ROOT" "$$SRC_DIR" --prefix=/ --libdir=lib`,
		`ninja -C "$$BUILD_ROOT"`,
		`DESTDIR="$$INSTALL_ROOT" meson install -C "$$BUILD_ROOT"`,
		`if [ -n "$${CAS_GRPC_ADDR:-}" ]; then`,
		`--srckey=`,
		`--platform=`,
		`rel="$${src##*elements/demo/}"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mesonRound2InstallBuild output missing %q\n--- got ---\n%s", want, got)
		}
	}
	// Single-platform shape: no exec_compatible_with, no path
	// prefix on outs, env-var fallback for --platform=.
	for _, unwanted := range []string{
		"exec_compatible_with",
		`name = "demo_install_`,
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("mesonRound2InstallBuild single-platform output unexpectedly contains %q\n%s", unwanted, got)
		}
	}
}

// TestMesonRound2InstallBuild_MultiPlatform asserts the multi-
// platform fan-out shape: per-platform name suffix, per-platform
// outs path prefix, exec_compatible_with carrying the constraint
// set, and the platform literal baked into --platform=.
func TestMesonRound2InstallBuild_MultiPlatform(t *testing.T) {
	elem := &element{Name: "demo"}
	plat := tracePlatform{
		Name:        "linux_x86_64",
		Constraints: []string{"@platforms//cpu:x86_64", "@platforms//os:linux"},
	}
	got := mesonRound2InstallBuild(elem, plat)

	for _, want := range []string{
		`name = "demo_install_linux_x86_64"`,
		`"linux_x86_64/install_tree.tar"`,
		`"linux_x86_64/trace.log"`,
		`exec_compatible_with`,
		`"@platforms//cpu:x86_64"`,
		`"@platforms//os:linux"`,
		`--platform="linux_x86_64"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mesonRound2InstallBuild multi-platform output missing %q\n--- got ---\n%s", want, got)
		}
	}
	// The single-platform env-var fallback shouldn't appear when
	// the platform is baked in literally.
	if strings.Contains(got, `--platform="$${CMAKE_TO_BAZEL_PLATFORM:-}"`) {
		t.Errorf("multi-platform mode still references CMAKE_TO_BAZEL_PLATFORM env-var fallback:\n%s", got)
	}
}
