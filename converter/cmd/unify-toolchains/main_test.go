package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain/probejson"
)

// TestRun_EndToEnd exercises the whole flow: fixture probe.json
// artifacts → unify-toolchains → repo-root with the four expected
// files. Uses the recorded hello-world fileapi reply as the per-cell
// content (one reply, multiple platforms × variants — sufficient
// for shape verification; full per-platform divergence is for the
// real-cmake render gate).
func TestRun_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	probeDir := filepath.Join(tmp, "cells")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load fileapi fixture: %v", err)
	}
	// Mint per-cell probe.json files for two platforms × three
	// variants. Each cell embeds the same hello-world reply (the
	// observe-fold below reduces this to a baseline + empty deltas
	// per platform, which is what we want for a shape test).
	platforms := []string{"linux_x86_64", "linux_aarch64"}
	variants := []toolchain.Variant{
		{Name: "baseline"},
		{Name: "debug", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Debug"}},
		{Name: "release", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Release"}},
	}
	for _, p := range platforms {
		for _, v := range variants {
			body, err := probejson.Marshal(v, r)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			name := p + "." + v.Name + ".probe.json"
			if err := os.WriteFile(filepath.Join(probeDir, name), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	platsPath := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(platsPath, []byte(`[
		{"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
		{"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}

	repoRoot := filepath.Join(tmp, "repo")
	args := []string{
		"--probe-cells", probeDir,
		"--platforms-json", platsPath,
		"--repo-root", repoRoot,
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Each expected file lands at the right path.
	for _, rel := range []string{
		"platforms/BUILD.bazel",
		"toolchains/BUILD.bazel",
		"toolchains/cc_toolchain_config.bzl",
		".bazelrc",
	} {
		full := filepath.Join(repoRoot, rel)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("missing: %s (%v)", rel, err)
		}
	}

	// .bazelrc carries the operator-overrides try-import + sanitizer
	// aliases + platform aliases.
	rc, err := os.ReadFile(filepath.Join(repoRoot, ".bazelrc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"try-import %workspace%/user.bazelrc",
		"build:asan --features=asan",
		"build:linux_x86_64 --platforms=//platforms:linux_x86_64",
		"build:linux_aarch64 --platforms=//platforms:linux_aarch64",
	} {
		if !strings.Contains(string(rc), want) {
			t.Errorf(".bazelrc missing %q\n%s", want, rc)
		}
	}

	// toolchains/BUILD.bazel: per-platform toolchain instances + the
	// register_toolchains("//toolchains:all") target.
	tcB, err := os.ReadFile(filepath.Join(repoRoot, "toolchains/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "linux_x86_64_toolchain"`,
		`name = "linux_aarch64_toolchain"`,
		`name = "all"`,
		`":linux_x86_64_toolchain"`,
		`":linux_aarch64_toolchain"`,
	} {
		if !strings.Contains(string(tcB), want) {
			t.Errorf("toolchains/BUILD.bazel missing %q", want)
		}
	}
}

// TestRegisterToolchainsCallPresent covers the tolerance of the
// MODULE.bazel banner check: substring-only matching produced
// false negatives on legitimate formatting variants. The regexp
// must accept whitespace + single/double quotes + multi-arg calls.
func TestRegisterToolchainsCallPresent(t *testing.T) {
	cases := map[string]bool{
		// canonical
		`register_toolchains("//toolchains:all")`: true,
		// single quotes
		`register_toolchains('//toolchains:all')`: true,
		// whitespace
		"register_toolchains(\n    \"//toolchains:all\",\n)": true,
		// extra args before / after
		`register_toolchains(":foo", "//toolchains:all")`: true,
		`register_toolchains("//toolchains:all", ":bar")`: true,
		// not present
		``:                                    false,
		`module(name="x")`:                    false,
		`register_toolchains("//foo:bar")`:    false,
		`register_toolchains("//toolchains")`: false, // wrong target
	}
	for body, want := range cases {
		if got := registerToolchainsCallPresent([]byte(body)); got != want {
			t.Errorf("registerToolchainsCallPresent(%q) = %v, want %v", body, got, want)
		}
	}
}

// TestRun_NoCellsForPlatform demonstrates the "skip-with-warning"
// behaviour: a platform listed in --platforms-json with no probe
// cells in --probe-cells is dropped from the output (with a
// stderr warning) rather than producing an empty cc_toolchain.
func TestRun_NoCellsForPlatform(t *testing.T) {
	tmp := t.TempDir()
	probeDir := filepath.Join(tmp, "cells")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	body, err := probejson.Marshal(toolchain.Variant{Name: "baseline"}, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, "linux_x86_64.baseline.probe.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Platforms include linux_aarch64 but no aarch64 cells exist.
	platsPath := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(platsPath, []byte(`[
		{"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
		{"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Join(tmp, "repo")
	args := []string{
		"--probe-cells", probeDir,
		"--platforms-json", platsPath,
		"--repo-root", repoRoot,
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	platsB, err := os.ReadFile(filepath.Join(repoRoot, "platforms/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(platsB), `name = "linux_aarch64"`) {
		t.Error("aarch64 platform was emitted despite having no probe cells")
	}
	if !strings.Contains(string(platsB), `name = "linux_x86_64"`) {
		t.Error("x86_64 platform missing")
	}
}
