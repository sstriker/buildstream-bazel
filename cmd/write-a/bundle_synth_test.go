package main

import (
	"strings"
	"testing"
)

// TestBundleSynthShell_PrefixCoverage pins the set of install-tree
// directories the cmake-config bundle synthesis walks. The list is
// load-bearing for cross-element find_package / pkg_check_modules
// resolution: a dep's .pc / cmake-config file that lives outside
// the walked set is silently dropped from the bundle, and the
// consumer's pass-2 configure can't find it. FDSDK is the
// motivating consumer; FDSDK is Debian-shaped, which means
// usr/lib/<triplet>/ multiarch is the real-world common case.
func TestBundleSynthShell_PrefixCoverage(t *testing.T) {
	got := bundleSynthShell()

	// Standard install layouts the bundle walks.
	stdPaths := []string{
		// Debian/Ubuntu native, meson default, cmake default.
		"lib/pkgconfig",
		// RedHat-family 64-bit.
		"lib64/pkgconfig",
		// --prefix=/usr (autotools default for some configure scripts).
		"usr/lib/pkgconfig",
		"usr/lib64/pkgconfig",
		// --prefix=/usr/local (autotools default for most configure scripts).
		"usr/local/lib/pkgconfig",
		"usr/local/lib64/pkgconfig",
		// Architecture-independent .pc files (autotools' default for noarch).
		"usr/share/pkgconfig",
		// Same set for cmake-config files.
		"lib/cmake",
		"lib64/cmake",
		"usr/lib/cmake",
		"usr/lib64/cmake",
		"usr/local/lib/cmake",
		"usr/local/lib64/cmake",
		"usr/share/cmake",
	}
	for _, p := range stdPaths {
		if !strings.Contains(got, p) {
			t.Errorf("bundleSynthShell missing standard prefix %q", p)
		}
	}

	// Debian/Ubuntu multiarch globs (e.g.
	// usr/lib/x86_64-linux-gnu/pkgconfig). The shell relies on
	// POSIX glob semantics — an unmatched glob expands to itself
	// literally and the [ -d ] guard then fails cleanly.
	multiarchGlobs := []string{
		`"$$INSTALL_ROOT"/usr/lib/*/pkgconfig`,
		`"$$INSTALL_ROOT"/lib/*/pkgconfig`,
		`"$$INSTALL_ROOT"/usr/lib/*/cmake`,
		`"$$INSTALL_ROOT"/lib/*/cmake`,
	}
	for _, g := range multiarchGlobs {
		if !strings.Contains(got, g) {
			t.Errorf("bundleSynthShell missing multiarch glob %q", g)
		}
	}

	// The output must be a single self-contained snippet that
	// defines + tars CONFIG_BUNDLE_DIR / CONFIG_BUNDLE_TAR. The
	// surrounding genrule cmd templates rely on $$CONFIG_BUNDLE_TAR
	// being defined when they invoke trace-publish.
	for _, contract := range []string{
		`export CONFIG_BUNDLE_DIR="$$(mktemp -d)"`,
		`export CONFIG_BUNDLE_TAR="$$(mktemp)"`,
		// Deterministic tar shape matches the install_tree.tar
		// shape used elsewhere in the round-2 templates.
		`tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner`,
		`-cf "$$CONFIG_BUNDLE_TAR" -C "$$CONFIG_BUNDLE_DIR" .`,
	} {
		if !strings.Contains(got, contract) {
			t.Errorf("bundleSynthShell missing contract piece %q", contract)
		}
	}
}

// TestBundleSynthShell_UsedByAllThreeHandlers asserts the helper's
// output appears verbatim in each of the three round-2 install
// genrules (pipeline / cmake / meson). Locks in the unification:
// if a future edit reverts one handler to its own walk, this test
// catches the divergence.
func TestBundleSynthShell_UsedByAllThreeHandlers(t *testing.T) {
	snippet := bundleSynthShell()

	// Pipeline handler emits via pipelineTracePublishStep.
	pipelineOut := pipelineTracePublishStep("elem", "", "")
	if !strings.Contains(pipelineOut, snippet) {
		t.Errorf("pipelineTracePublishStep does not embed bundleSynthShell() verbatim")
	}

	// cmake and meson handlers emit via cmakeRound2InstallBuild /
	// mesonRound2InstallBuild — only need the element name.
	elem := &element{Name: "demo"}
	cmakeOut := cmakeRound2InstallBuild(elem, tracePlatform{})
	if !strings.Contains(cmakeOut, snippet) {
		t.Errorf("cmakeRound2InstallBuild does not embed bundleSynthShell() verbatim")
	}

	mesonOut := mesonRound2InstallBuild(elem, tracePlatform{})
	if !strings.Contains(mesonOut, snippet) {
		t.Errorf("mesonRound2InstallBuild does not embed bundleSynthShell() verbatim")
	}
}
