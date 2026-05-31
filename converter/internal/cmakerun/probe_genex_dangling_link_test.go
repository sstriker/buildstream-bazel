package cmakerun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProbeGenex_DanglingLinkInterface_LiveCMake pins the fix for the
// abseil-under-cmake-4 regression: probe-genex.cmake must not fatal-error
// the generate step on a target whose link interface names a `::`
// (namespaced) target that doesn't exist.
//
// The fixture mirrors abseil's shape — a non-TESTONLY INTERFACE library
// (`dangling_consumer`) whose `target_link_libraries(... INTERFACE
// absl::nonexistent_testonly)` references a target that was never created
// (abseil's `heterogeneous_lookup_testing` -> `absl::test_instance_tracker`
// when testing is off). cmake tolerates the dangling reference until
// something forces the interface to evaluate; the probe's
// `file(GENERATE $<TARGET_PROPERTY:t,INTERFACE_LINK_LIBRARIES>)` would do
// exactly that and abort the whole configure with "the target was not
// found". The fix skips probing such a target (cmakerun.ReadGenexProbe
// tolerates a missing per-target dir, and the lifter falls back to legacy
// genex handling for it).
//
// Drives cmake directly with the hook injected — the failure mode is in
// cmake's generation step, so we assert on the exit code. Skips cleanly
// without cmake >= 3.24 (the CMAKE_PROJECT_TOP_LEVEL_INCLUDES floor).
func TestProbeGenex_DanglingLinkInterface_LiveCMake(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping probe-genex dangling-link live test")
	}
	major, minor, _, err := AssertVersion(context.Background())
	if err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	if major < 3 || (major == 3 && minor < 24) {
		t.Skipf("cmake %d.%d below probe-genex floor (3.24+); skipping", major, minor)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/probe-genex-dangling-link")
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

	// Cover both generator modes: the probe's INTERFACE_LINK_LIBRARIES
	// file(GENERATE) forces the dangling-`::`-dep evaluation regardless of
	// config count (single-config Ninja emits one OUTPUT, Ninja
	// Multi-Config emits one per $<CONFIG>) — both fatal-errored pre-fix.
	// The skip lives in the per-target loop preamble, ahead of any
	// per-config emit, so it must hold for both. The default survey path
	// is single-config; --build-types switches to multi-config.
	for _, tc := range []struct {
		name      string
		generator string
		extraArgs []string
	}{
		{"single-config", "Ninja", []string{"-DCMAKE_BUILD_TYPE=Release"}},
		{"multi-config", "Ninja Multi-Config", []string{"-DCMAKE_CONFIGURATION_TYPES=Release;Debug"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buildDir := t.TempDir()
			args := []string{"-S", src, "-B", buildDir, "-G", tc.generator}
			args = append(args, tc.extraArgs...)
			args = append(args, "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+hook)
			cmd := exec.CommandContext(context.Background(), "cmake", args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("cmake configure+generate with probe-genex hook failed on the dangling-link fixture (the fix should skip the bad target, not abort): %v\n%s", err, out)
			}

			// The real, buildable target must still be probed; the
			// dangling consumer must be skipped (not present).
			genexRoot := filepath.Join(buildDir, ProbeGenexDirname)
			if _, err := os.Stat(filepath.Join(genexRoot, "real")); err != nil {
				t.Errorf("expected the real STATIC_LIBRARY target to be probed, but its dir is missing: %v", err)
			}
			if _, err := os.Stat(filepath.Join(genexRoot, "dangling_consumer")); !os.IsNotExist(err) {
				t.Errorf("expected dangling_consumer to be skipped (dir absent), but stat returned err=%v", err)
			}
		})
	}
}
