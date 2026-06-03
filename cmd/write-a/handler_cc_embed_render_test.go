package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ccEmbedRender holds the rendered artifacts both projects produce, so
// a test can assert the SYMMETRIC tools/ staging contract (the binary +
// its exports_files entry must land in project A AND project B, since
// either side can host the cc_embed rule whose //tools:cc-embed tool
// label must resolve).
type ccEmbedRender struct {
	genrule    string // project A's element converter genrule BUILD
	toolsA     string // project A's tools/BUILD.bazel
	toolsB     string // project B's tools/BUILD.bazel
	outA, outB string // project out dirs, for staged-binary checks
}

// renderCmakeProjectWithCCEmbed renders BOTH projects for a trivial
// kind:cmake element (honoring whatever cmakeConfig.ccEmbedBin the
// caller set) and returns their artifacts.
func renderCmakeProjectWithCCEmbed(t *testing.T) ccEmbedRender {
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
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, fakeConvertBin(t, tmp)); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	gr, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	tbA, err := os.ReadFile(filepath.Join(outA, "tools/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	tbB, err := os.ReadFile(filepath.Join(outB, "tools/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	return ccEmbedRender{
		genrule: string(gr),
		toolsA:  string(tbA),
		toolsB:  string(tbB),
		outA:    outA,
		outB:    outB,
	}
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

	r := renderCmakeProjectWithCCEmbed(t)

	if !strings.Contains(r.genrule, "--lift-cc-embed=true") {
		t.Errorf("converter genrule missing --lift-cc-embed=true:\n%s", r.genrule)
	}
	// The staging is symmetric: the binary + its exports_files entry
	// must land in BOTH projects so //tools:cc-embed resolves wherever
	// the cc_embed rule ends up.
	for _, p := range []struct {
		side, tools, out string
	}{
		{"A", r.toolsA, r.outA},
		{"B", r.toolsB, r.outB},
	} {
		if !strings.Contains(p.tools, `"cc-embed"`) {
			t.Errorf("project %s tools/BUILD.bazel missing cc-embed export:\n%s", p.side, p.tools)
		}
		info, err := os.Stat(filepath.Join(p.out, "tools", "cc-embed"))
		if err != nil {
			t.Fatalf("cc-embed not staged into project %s tools/: %v", p.side, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("staged cc-embed in project %s is not executable: mode %v", p.side, info.Mode())
		}
	}
}

// TestWriter_CCEmbed_OffShapeUnchanged pins that the default (flag off)
// render threads no --lift-cc-embed and stages no cc-embed tool — the
// byte-shape guarantee for the untouched path.
func TestWriter_CCEmbed_OffShapeUnchanged(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.ccEmbedBin = ""
	t.Cleanup(func() { cmakeConfig = prev })

	r := renderCmakeProjectWithCCEmbed(t)

	if strings.Contains(r.genrule, "lift-cc-embed") {
		t.Errorf("flag-off genrule unexpectedly mentions lift-cc-embed:\n%s", r.genrule)
	}
	for _, p := range []struct {
		side, tools, out string
	}{
		{"A", r.toolsA, r.outA},
		{"B", r.toolsB, r.outB},
	} {
		if strings.Contains(p.tools, "cc-embed") {
			t.Errorf("flag-off project %s tools/BUILD.bazel unexpectedly exports cc-embed:\n%s", p.side, p.tools)
		}
		if _, err := os.Stat(filepath.Join(p.out, "tools", "cc-embed")); !os.IsNotExist(err) {
			t.Errorf("flag-off project %s unexpectedly staged a cc-embed binary (err=%v)", p.side, err)
		}
	}
}
