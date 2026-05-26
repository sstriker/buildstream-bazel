package cmakerun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProbeGenex_UtilityTargets_LiveCMake exercises the
// affirmative-type gate in probe-genex.cmake against a real cmake
// project that mixes artifact-producing targets (STATIC_LIBRARY)
// with non-artifact targets (UTILITY, ALIAS, INTERFACE_LIBRARY).
//
// The bug this test pins: round-1 of PR #227 (commit 2f67f4b)
// flipped the `$<TARGET_FILE:tgt>` evaluation gate from an
// exclusion list (NOT INTERFACE_LIBRARY) to an affirmative
// inclusion of artifact-producing types. Pre-fix, cmake's
// generation step fatal-errored on UTILITY targets with "Target
// <name> is not an executable or library" — boost's tests /
// check / etc. tripped this 48 times → conversion aborted.
//
// Post-fix, the gate accepts only EXECUTABLE / SHARED_LIBRARY /
// STATIC_LIBRARY / MODULE_LIBRARY / OBJECT_LIBRARY; everything
// else falls through safely. cmake's generation step succeeds and
// the genex probe outputs only contain the artifact-producing
// target's resolved values.
//
// The test runs cmake live (requires cmake >= 3.24 for
// CMAKE_PROJECT_TOP_LEVEL_INCLUDES). It skips cleanly when cmake
// isn't on PATH.
func TestProbeGenex_UtilityTargets_LiveCMake(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping probe-genex UTILITY live test")
	}
	major, minor, _, err := AssertVersion(context.Background())
	if err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	// probe-genex.cmake is injected via CMAKE_PROJECT_TOP_LEVEL_INCLUDES
	// which requires cmake 3.24+ (the architectural floor for the
	// hook). Earlier cmakes would silently drop the -D.
	if major < 3 || (major == 3 && minor < 24) {
		t.Skipf("cmake %d.%d below probe-genex floor (3.24+); skipping", major, minor)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/probe-genex-utility")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	hook, err := filepath.Abs("probe-genex.cmake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("probe-genex.cmake missing alongside test: %v", err)
	}

	buildDir := t.TempDir()
	// Drive cmake directly with the probe-genex hook injected.
	// We bypass cmakerun.Configure because the failure mode we're
	// pinning is in cmake's *generation* step, which Configure
	// would surface as a non-zero exit; cleanest to invoke cmake
	// directly and assert on the exit code + stderr shape.
	cmd := exec.CommandContext(context.Background(), "cmake",
		"-S", src,
		"-B", buildDir,
		"-G", "Ninja",
		"-DCMAKE_BUILD_TYPE=Release",
		"-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+hook,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmake failed with probe-genex hook on UTILITY targets:\n%s\nerror: %v", out, err)
	}

	// Verify the probe outputs are present for the artifact-
	// producing target AND absent for the non-artifact targets —
	// that's the affirmative-gate semantics.
	realFile := filepath.Join(buildDir, "cmake-to-bazel.genex", "realtarget", "file.txt")
	if _, err := os.Stat(realFile); err != nil {
		t.Errorf("expected realtarget probe file at %q: %v", realFile, err)
	}
	utilFile := filepath.Join(buildDir, "cmake-to-bazel.genex", "noisy_utility", "file.txt")
	if _, err := os.Stat(utilFile); err == nil {
		t.Errorf("noisy_utility probe file.txt should NOT exist (gate skipped UTILITY); found %q", utilFile)
	}
	// type.txt is emitted for ALL targets the gate sees (it's the
	// outer probe used as the gating signal). UTILITY targets'
	// type.txt SHOULD be present.
	utilType := filepath.Join(buildDir, "cmake-to-bazel.genex", "noisy_utility", "type.txt")
	if _, err := os.Stat(utilType); err != nil {
		t.Errorf("expected noisy_utility type.txt at %q: %v", utilType, err)
	}
}
