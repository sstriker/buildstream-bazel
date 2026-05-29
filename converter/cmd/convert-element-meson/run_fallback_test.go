package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
)

// TestRun_FallbackPropagatesOriginalTier1WhenPlaceholderHasNoTargets
// covers the post-emit zero-targets propagation path in run():
//
// Setup:
//   - intro-targets.json declares one target inside a subproject —
//     Lower() refuses with unsupported-meson-subproject (a typed
//     Tier-1).
//   - intro-install_plan.json's `targets` is non-empty (passes
//     the pre-emit length check) but every entry is itself
//     subproject-tagged — emitFallbackPlaceholder filters them all
//     out and returns a package with zero Targets.
//
// Without the post-emit `len(pkg.Targets) > 0` check, run() would
// land an empty BUILD on disk and silently hide the original
// refusal. The fix propagates the original Tier-1 in that case;
// this test pins the contract.
func TestRun_FallbackPropagatesOriginalTier1WhenPlaceholderHasNoTargets(t *testing.T) {
	tmp := t.TempDir()
	infoDir := filepath.Join(tmp, "build", "meson-info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// intro-targets: one target in subproject "foo" → Lower returns
	// unsupported-meson-subproject.
	subprojName := "foo"
	targets := []Target{{
		Name:       "libfoo",
		ID:         "libfoo@sta",
		Type:       "static library",
		Subproject: &subprojName,
		Filename:   []string{"/bd/subprojects/foo/libfoo.a"},
	}}
	writeJSON(t, filepath.Join(infoDir, "intro-targets.json"), targets)
	writeJSON(t, filepath.Join(infoDir, "intro-projectinfo.json"), ProjectInfo{Name: "p"})
	writeJSON(t, filepath.Join(infoDir, "intro-buildoptions.json"), []BuildOption{
		{Name: "libdir", Section: "directory", Value: "lib"},
	})
	// intro-install_plan: non-empty `targets` so the pre-emit
	// gate passes; but every entry is subproject-tagged so the
	// emitter filters them all out, returning zero stubs.
	plan := InstallPlan{
		Targets: map[string]InstallPlanEntry{
			"/bd/subprojects/foo/libfoo.a": {
				Destination: "{libdir_static}/libfoo.a",
				Tag:         "devel",
				Subproject:  &subprojName,
			},
		},
	}
	writeJSON(t, filepath.Join(infoDir, "intro-install_plan.json"), plan)

	outBuild := filepath.Join(tmp, "BUILD.out")
	a := args{
		sourceRoot:                "/src",
		infoDir:                   infoDir,
		outBuild:                  outBuild,
		unsupportedTargetFallback: true,
	}
	err := run(a)
	if err == nil {
		t.Fatalf("run() succeeded but should have propagated the original Tier-1; outBuild contents:\n%s", readOrEmpty(t, outBuild))
	}
	var tier1 *failure.Error
	if !errors.As(err, &tier1) {
		t.Fatalf("run() returned non-Tier-1 error %T: %v", err, err)
	}
	if tier1.Code != unsupportedMesonSubproject {
		t.Errorf("propagated Tier-1 code = %q, want %q", tier1.Code, unsupportedMesonSubproject)
	}
	// The empty BUILD should NOT have been written (run returns
	// before the WriteFile call when err != nil).
	if _, statErr := os.Stat(outBuild); statErr == nil {
		t.Errorf("BUILD.out was written despite propagated refusal; contents:\n%s", readOrEmpty(t, outBuild))
	}
}

// TestRun_FallbackEmitsPlaceholderForValidInstallPlan covers the
// happy path of the dispatch: a Tier-1 refusal with a non-empty
// install plan + non-subproject entries → emitter produces a
// non-empty placeholder → BUILD.bazel.out lands on disk and run()
// returns nil. Pins the wedge between the test above (zero stubs
// → propagate) and a no-refusal run (no fallback needed).
func TestRun_FallbackEmitsPlaceholderForValidInstallPlan(t *testing.T) {
	tmp := t.TempDir()
	infoDir := filepath.Join(tmp, "build", "meson-info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subprojName := "subdep"
	// One subproject target triggers the refusal; the install plan
	// also carries a top-level (non-subproject) target so the
	// emitter has something to anchor against.
	targets := []Target{{
		Name:       "libsub",
		ID:         "libsub@sta",
		Type:       "static library",
		Subproject: &subprojName,
		Filename:   []string{"/bd/subprojects/subdep/libsub.a"},
	}}
	writeJSON(t, filepath.Join(infoDir, "intro-targets.json"), targets)
	writeJSON(t, filepath.Join(infoDir, "intro-projectinfo.json"), ProjectInfo{Name: "p"})
	writeJSON(t, filepath.Join(infoDir, "intro-buildoptions.json"), []BuildOption{
		{Name: "libdir", Section: "directory", Value: "lib"},
	})
	plan := InstallPlan{
		Targets: map[string]InstallPlanEntry{
			"/bd/libmain.a": {
				Destination: "{libdir_static}/libmain.a",
				Tag:         "devel",
			},
			"/bd/subprojects/subdep/libsub.a": {
				Destination: "{libdir_static}/libsub.a",
				Tag:         "devel",
				Subproject:  &subprojName,
			},
		},
	}
	writeJSON(t, filepath.Join(infoDir, "intro-install_plan.json"), plan)

	outBuild := filepath.Join(tmp, "BUILD.out")
	a := args{
		sourceRoot:                "/src",
		infoDir:                   infoDir,
		outBuild:                  outBuild,
		unsupportedTargetFallback: true,
	}
	if err := run(a); err != nil {
		t.Fatalf("run() = %v, want nil (fallback should have produced a non-empty placeholder)", err)
	}
	body := readOrEmpty(t, outBuild)
	if body == "" {
		t.Fatalf("BUILD.out empty after successful fallback run")
	}
	for _, want := range []string{
		`name = "main"`,
		`static_library = ":_pick_lib_libmain_a"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fallback BUILD missing marker %q\n--- BUILD ---\n%s", want, body)
		}
	}
	// The subproject target must be filtered out.
	if strings.Contains(body, "libsub") {
		t.Errorf("subproject target leaked into fallback BUILD:\n%s", body)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readOrEmpty(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
