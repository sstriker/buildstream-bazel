package cmakerun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestConfigure_PolicyFloorRescue is the end-to-end coverage for the
// automatic cmake-4 policy-floor retry. It drives Configure against a
// fixture whose CMakeLists.txt declares cmake_minimum_required(VERSION 3.0)
// — below the 3.5 floor cmake 4.x dropped compatibility for. Under cmake
// 4.x the first configure pass fatal-errors; Configure detects the
// floor-removal sentinel and retries once with
// CMAKE_POLICY_VERSION_MINIMUM=3.5, so the call ultimately succeeds.
//
// Under cmake 3.x the floor is honoured directly (deprecation warning
// only, no fatal), so there's nothing to rescue and the assertion (a
// successful Configure) holds without the retry firing. The test therefore
// runs on any cmake at or above the codemodel-v2 floor and just asserts
// success — the retry path is what makes that true on cmake 4.x, and the
// matcher + env-append units pin the mechanism precisely.
func TestConfigure_PolicyFloorRescue(t *testing.T) {
	ctx := context.Background()

	major, minor, patch, err := AssertVersion(ctx)
	if err != nil {
		t.Skipf("cmake unavailable or below codemodel-v2 floor: %v", err)
	}

	srcRoot, err := filepath.Abs("../../testdata/sample-projects/policy-floor-removed")
	if err != nil {
		t.Fatalf("abs src: %v", err)
	}
	buildDir, err := os.MkdirTemp("", "policy-floor-e2e-*")
	if err != nil {
		t.Fatalf("tmp build dir: %v", err)
	}
	defer os.RemoveAll(buildDir)

	if _, err := Configure(ctx, Options{
		SourceRoot: srcRoot,
		BuildDir:   buildDir,
	}); err != nil {
		t.Fatalf("Configure against the sub-3.5-floor fixture failed under cmake %d.%d.%d; "+
			"the policy-floor retry should have rescued it: %v", major, minor, patch, err)
	}
}
