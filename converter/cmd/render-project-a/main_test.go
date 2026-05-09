package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadPlatforms_Schema covers the platforms-json shape.
func TestLoadPlatforms_Schema(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "platforms.json")
	body := []byte(`[
		{"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
		{"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
	]`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadPlatforms(path)
	if err != nil {
		t.Fatalf("loadPlatforms: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 platforms; got %d", len(got))
	}
	if got[0].Name != "linux_x86_64" || got[1].Name != "linux_aarch64" {
		t.Errorf("names: %v", got)
	}
	if len(got[0].Constraints) != 2 {
		t.Errorf("expected 2 constraints; got %v", got[0].Constraints)
	}
}

// TestEndToEnd_AgainstCanonicalPresets exercises the full flow:
// the fixture CMakePresets.json (Stage 3 catalog) + a 2-platform
// platforms.json render to a BUILD.bazel under a temp out dir.
// This is the contract Stage 5's unifier consumes downstream.
func TestEndToEnd_AgainstCanonicalPresets(t *testing.T) {
	tmp := t.TempDir()
	platformsPath := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(platformsPath, []byte(`[
		{"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
		{"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmp, "out")
	args := []string{
		"--out", outDir,
		"--variants-from", "../../testdata/toolchain-probe/CMakePresets.json",
		"--platforms-json", platformsPath,
	}

	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(outDir, "BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	// Two platforms × the canonical preset count: every cell shows
	// up. We don't pin the exact count (the fixture is allowed to
	// grow); we sample a few we know are stable.
	for _, want := range []string{
		`name = "linux_x86_64.baseline"`,
		`name = "linux_x86_64.asan"`,
		`name = "linux_aarch64.coverage"`,
		`@platforms//cpu:x86_64`,
		`@platforms//cpu:arm64`,
		`name = "all_probes"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("BUILD.bazel missing %q", want)
		}
	}
}
