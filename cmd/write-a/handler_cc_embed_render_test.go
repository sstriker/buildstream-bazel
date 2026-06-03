package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderCmakeProjectWithCCEmbed renders BOTH projects for a trivial
// kind:cmake element with --cc-embed-bin wired (a fake staged binary),
// returning the element's project-A genrule BUILD, project A's
// tools/BUILD.bazel, and project A's out dir (for staged-binary checks).
func renderCmakeProjectWithCCEmbed(t *testing.T) (genrule, toolsBuild, outA string) {
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
	outA = filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, fakeConvertBin(t, tmp)); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	gr, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	tb, err := os.ReadFile(filepath.Join(outA, "tools/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	return string(gr), string(tb), outA
}

// fakeCCEmbedBin stages a fake cc-embed binary and returns its path,
// so stageCCEmbedTool has a real file to copy.
func fakeCCEmbedBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cc-embed")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestWriter_CCEmbed_LiftWired covers --cc-embed-bin: the converter
// genrule threads --lift-cc-embed=true, the binary stages into
// project A's tools/ and is exported, so the cc_embed rule's
// //tools:cc-embed tool label resolves downstream.
func TestWriter_CCEmbed_LiftWired(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.ccEmbedBin = fakeCCEmbedBin(t)
	t.Cleanup(func() { cmakeConfig = prev })

	genrule, toolsBuild, outA := renderCmakeProjectWithCCEmbed(t)

	if !strings.Contains(genrule, "--lift-cc-embed=true") {
		t.Errorf("converter genrule missing --lift-cc-embed=true:\n%s", genrule)
	}
	if !strings.Contains(toolsBuild, `"cc-embed"`) {
		t.Errorf("tools/BUILD.bazel missing cc-embed export:\n%s", toolsBuild)
	}
	staged := filepath.Join(outA, "tools", "cc-embed")
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("cc-embed not staged into tools/: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("staged cc-embed is not executable: mode %v", info.Mode())
	}
}

// TestWriter_CCEmbed_OffShapeUnchanged pins that the default (flag off)
// render threads no --lift-cc-embed and stages no cc-embed tool — the
// byte-shape guarantee for the untouched path.
func TestWriter_CCEmbed_OffShapeUnchanged(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.ccEmbedBin = ""
	t.Cleanup(func() { cmakeConfig = prev })

	genrule, toolsBuild, outA := renderCmakeProjectWithCCEmbed(t)

	if strings.Contains(genrule, "lift-cc-embed") {
		t.Errorf("flag-off genrule unexpectedly mentions lift-cc-embed:\n%s", genrule)
	}
	if strings.Contains(toolsBuild, "cc-embed") {
		t.Errorf("flag-off tools/BUILD.bazel unexpectedly exports cc-embed:\n%s", toolsBuild)
	}
	if _, err := os.Stat(filepath.Join(outA, "tools", "cc-embed")); !os.IsNotExist(err) {
		t.Errorf("flag-off run unexpectedly staged a cc-embed binary (err=%v)", err)
	}
}
