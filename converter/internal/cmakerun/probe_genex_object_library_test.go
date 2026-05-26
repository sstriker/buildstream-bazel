package cmakerun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestProbeGenex_ObjectLibrary_LiveCMake exercises the
// OBJECT_LIBRARY emission gate in probe-genex.cmake against a
// real cmake project that has both an OBJECT_LIBRARY target
// (which gets the `objects.txt` probe) and a consumer
// STATIC_LIBRARY target (which doesn't).
//
// The contract this test pins: the OBJECT_LIBRARY gate
// (`_CMTB_TYPE STREQUAL "OBJECT_LIBRARY"`) emits one
// `objects.txt` file per OBJECT_LIBRARY target containing
// cmake's authoritative resolution of `$<TARGET_OBJECTS:t>` —
// the semicolon-separated list of .o paths cmake's generator
// produces. Non-OBJECT_LIBRARY targets (here the consumer
// STATIC_LIBRARY) don't get the `objects.txt` emission.
//
// Pre-Phase-3 the offline lifter routed `$<TARGET_OBJECTS:t>`
// through UnsupportedError → (b)/legacy fallback. The probe-
// genex hook lets cmake answer; the reader (ReadGenexProbe)
// surfaces the value on GenexProbe.Objects so
// buildGenexTargets can populate genexeval.TargetInfo.Objects
// and the (a) lift evaluates the TARGET_OBJECTS genex directly
// at convert time (with --target-objects= overriding at Bazel
// time per the cross-machine wire).
//
// The test runs cmake live (requires cmake >= 3.24 for
// CMAKE_PROJECT_TOP_LEVEL_INCLUDES). It skips cleanly when cmake
// isn't on PATH.
func TestProbeGenex_ObjectLibrary_LiveCMake(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping probe-genex OBJECT_LIBRARY live test")
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

	src, err := filepath.Abs("../../testdata/sample-projects/object-library")
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
	// We bypass cmakerun.Configure because we want the test to
	// fail loudly on any probe-genex regression that affects
	// generation; Configure's error-reporting layer would lose
	// the cmake stderr signal that's most useful for debugging.
	cmd := exec.CommandContext(context.Background(), "cmake",
		"-S", src,
		"-B", buildDir,
		"-G", "Ninja",
		"-DCMAKE_BUILD_TYPE=Release",
		"-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+hook,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmake failed with probe-genex hook on OBJECT_LIBRARY targets:\n%s\nerror: %v", out, err)
	}

	// Verify the objects.<config>.txt emission landed for the
	// OBJECT_LIBRARY target. The fixture defines `objlib_obj` as
	// the OBJECT lib; `objlib_archive` is the consumer
	// STATIC_LIBRARY (no objects). Per-config OUTPUT path comes
	// from probe-genex.cmake's `$<CONFIG>` infix (single-config
	// Ninja resolves $<CONFIG> to CMAKE_BUILD_TYPE — "Release"
	// here).
	objLibObjects := filepath.Join(buildDir, "cmake-to-bazel.genex", "objlib_obj", "objects.Release.txt")
	body, err := os.ReadFile(objLibObjects)
	if err != nil {
		t.Fatalf("expected objects.Release.txt for OBJECT_LIBRARY at %q: %v", objLibObjects, err)
	}
	// cmake resolves $<TARGET_OBJECTS:objlib_obj> to a semicolon-
	// separated list of .o paths — one per source file in the
	// OBJECT lib (src/a.c, src/b.c → two .o files). The exact
	// paths depend on cmake's CMakeFiles layout; assert on the
	// shape (semicolon separator + at least one .o suffix).
	bodyStr := string(body)
	if bodyStr == "" {
		t.Errorf("objects.txt body empty for OBJECT_LIBRARY target")
	}
	if !strings.Contains(bodyStr, ".o") {
		t.Errorf("objects.txt body %q has no .o suffix; cmake's TARGET_OBJECTS didn't resolve", bodyStr)
	}
	// Two source files in the fixture → two .o paths separated by
	// `;`. cmake's serialization is deterministic for this shape.
	if !strings.Contains(bodyStr, ";") {
		t.Errorf("objects.txt body %q missing `;` separator (expected two .o paths from src/a.c + src/b.c)", bodyStr)
	}

	// Consumer STATIC_LIBRARY (objlib_archive) should NOT get an
	// objects.<config>.txt emission — the gate excludes
	// non-OBJECT_LIBRARY types. The TARGET_FILE family files do
	// land (it's an artifact-producing target), so we use
	// objects.<config>.txt as the distinguishing check.
	consumerObjects := filepath.Join(buildDir, "cmake-to-bazel.genex", "objlib_archive", "objects.Release.txt")
	if _, err := os.Stat(consumerObjects); err == nil {
		t.Errorf("STATIC_LIBRARY consumer should NOT have objects.Release.txt (gate skipped non-OBJECT_LIBRARY); found %q", consumerObjects)
	}
	// type.txt for the consumer should still exist — the outer
	// probe is unconditional. Used here as a positive cross-check
	// that the consumer target was visible to the probe at all.
	consumerType := filepath.Join(buildDir, "cmake-to-bazel.genex", "objlib_archive", "type.txt")
	if _, err := os.Stat(consumerType); err != nil {
		t.Errorf("expected consumer type.txt at %q: %v", consumerType, err)
	}
}
