package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriter_BuildTypes_ThreadsConverterFlag covers write-a's project-A
// render when --build-types is set: every kind:cmake converter genrule
// threads --build-types=<list> so cmake runs under Ninja Multi-Config and
// BUILD.bazel.out carries the //config:<name> select() arms.
func TestWriter_BuildTypes_ThreadsConverterFlag(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.buildTypes = []string{"Debug", "Release", "RelWithDebInfo"}
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	if !strings.Contains(body, "--build-types=Debug,Release,RelWithDebInfo") {
		t.Errorf("converter genrule missing --build-types flag:\n%s", body)
	}
}

// TestWriter_BuildTypes_DefaultElidesFlag pins byte-stability: with no
// --build-types, the converter cmd carries no --build-types flag (legacy
// single-config callsites stay unchanged).
func TestWriter_BuildTypes_DefaultElidesFlag(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.buildTypes = nil
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	if strings.Contains(body, "--build-types") {
		t.Errorf("default render unexpectedly carries --build-types:\n%s", body)
	}
}

// TestWriter_BuildTypes_ConfigPackageInProjectB covers the //config package
// render: when --build-types is set, project B (where the staged
// BUILD.bazel.out is loaded) gets a //config package with a string_flag and
// one config_setting per config, so the select() arm labels resolve.
func TestWriter_BuildTypes_ConfigPackageInProjectB(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.buildTypes = []string{"Debug", "Release", "RelWithDebInfo"}
	t.Cleanup(func() { cmakeConfig = prev })

	outB := renderCmakeProjectB(t)
	cfg, err := os.ReadFile(filepath.Join(outB, "config", "BUILD.bazel"))
	if err != nil {
		t.Fatalf("project B missing //config/BUILD.bazel: %v", err)
	}
	got := string(cfg)
	for _, want := range []string{
		`load("@bazel_skylib//rules:common_settings.bzl", "string_flag")`,
		`name = "build_type"`,
		`build_setting_default = "debug"`,
		`name = "debug"`,
		`name = "release"`,
		`name = "relwithdebinfo"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("//config/BUILD.bazel missing %q\n%s", want, got)
		}
	}

	// The //config package's string_flag needs bazel_skylib in project
	// B's module graph, or the //config:build_type load() can't resolve.
	mod, err := os.ReadFile(filepath.Join(outB, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), `bazel_dep(name = "bazel_skylib"`) {
		t.Errorf("project B MODULE.bazel missing bazel_skylib dep (needed by //config string_flag):\n%s", mod)
	}
}

// TestWriter_BuildTypes_NoConfigPackageWhenUnset pins that single-config
// renders don't emit a //config package (byte-stable).
func TestWriter_BuildTypes_NoConfigPackageWhenUnset(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.buildTypes = nil
	t.Cleanup(func() { cmakeConfig = prev })

	outB := renderCmakeProjectB(t)
	if _, err := os.Stat(filepath.Join(outB, "config", "BUILD.bazel")); !os.IsNotExist(err) {
		t.Errorf("single-config render unexpectedly emitted //config (err=%v)", err)
	}
	// skylib IS expected even single-config now: kind:cmake elements may
	// emit the file(GENERATE) write_file bake, which loads
	// @bazel_skylib//rules:write_file.bzl (gated on hasKind cmake,
	// independent of multi-config). The invariant that must hold is that
	// the multi-config path and the write_file path don't BOTH emit it —
	// the `||` guard collapses to exactly one bazel_dep line.
	mod, err := os.ReadFile(filepath.Join(outB, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(mod), `bazel_dep(name = "bazel_skylib"`); n != 1 {
		t.Errorf("expected exactly one bazel_skylib bazel_dep (no duplicate), got %d:\n%s", n, mod)
	}
}

// renderCmakeProjectB renders project B for a single trivial kind:cmake
// element and returns project B's output dir.
func renderCmakeProjectB(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(demo C)\nadd_library(thelib STATIC lib.c)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "lib.c"), []byte("int f(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "demo.bst")
	if err := os.WriteFile(bst, []byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	return outB
}

// TestWriter_BuildTypes_SplitPackages_ThreadsConverterArg pins that
// --build-types also threads through the --split-packages delivery shape
// (the cmake_split_convert rule's converter_args), not just the genrule.
func TestWriter_BuildTypes_SplitPackages_ThreadsConverterArg(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.splitPackages = true
	cmakeConfig.buildTypes = []string{"Debug", "Release"}
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	if !strings.Contains(body, "--build-types=Debug,Release") {
		t.Errorf("split-packages converter_args missing --build-types:\n%s", body)
	}
}

// TestWriter_BuildTypes_Auto covers --build-types=auto: write-a threads the
// literal --build-types=auto into the converter genrule (config detection
// stays in project A's conversion action — write-a never runs cmake) and
// emits the //config package over cmake's standard config set.
func TestWriter_BuildTypes_Auto(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.autoBuildTypes = true
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	if !strings.Contains(body, "--build-types=auto") {
		t.Errorf("auto mode: converter genrule missing --build-types=auto:\n%s", body)
	}

	outB := renderCmakeProjectB(t)
	cfg, err := os.ReadFile(filepath.Join(outB, "config", "BUILD.bazel"))
	if err != nil {
		t.Fatalf("auto mode: project B missing //config/BUILD.bazel: %v", err)
	}
	got := string(cfg)
	// //config covers cmake's standard set (the per-project detected set is
	// a subset, resolved at build time in A's conversion action).
	for _, want := range []string{
		`name = "debug"`, `name = "release"`, `name = "relwithdebinfo"`, `name = "minsizerel"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("auto //config missing %q\n%s", want, got)
		}
	}

	// The skylib dep must be added in auto mode too — the //config
	// string_flag needs it. (Regression guard: the gate keyed on the
	// explicit buildTypes list, missing auto, until the multiConfigEnabled
	// predicate unified them.)
	mod, err := os.ReadFile(filepath.Join(outB, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), `bazel_dep(name = "bazel_skylib"`) {
		t.Errorf("auto mode: project B MODULE.bazel missing bazel_skylib dep:\n%s", mod)
	}
}
