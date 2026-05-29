package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderCmakeProjectA renders project A for a single trivial kind:cmake
// element and returns the element's project-A BUILD.bazel text.
func renderCmakeProjectA(t *testing.T) string {
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
	body, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestWriter_SplitPackages_GenruleShape covers write-a's project-A
// render when --split-packages is on: the converter genrule threads
// --split-packages, declares the single build-packages.tar output
// (not BUILD.bazel.out), tars the per-sub-package tree, and the
// build_bazel filegroup points at the tar. The default (off) keeps
// the single BUILD.bazel.out shape.
func TestWriter_SplitPackages_GenruleShape(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.splitPackages = true
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	for _, want := range []string{
		// The threaded flag line (indented), not the bare token that
		// also appears in the template's explanatory comment.
		"\n            --split-packages",
		`"build-packages.tar",`,
		`tar -cf "$(location build-packages.tar)" -C "$$PKGTREE" .`,
		`--out-build="$$PKGTREE/BUILD.bazel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("split-mode project A BUILD missing %q\n%s", want, body)
		}
	}
	// In split mode the single-file output is replaced, not added.
	if strings.Contains(body, `"BUILD.bazel.out",`) {
		t.Errorf("split-mode project A BUILD still declares BUILD.bazel.out:\n%s", body)
	}
}

// TestWriter_SplitPackages_OffShapeUnchanged pins that the default
// (flag off) render keeps the single BUILD.bazel.out output and emits
// neither --split-packages nor build-packages.tar — the byte-shape
// guarantee for the untouched path.
func TestWriter_SplitPackages_OffShapeUnchanged(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.splitPackages = false
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	for _, want := range []string{
		`"BUILD.bazel.out",`,
		`--out-build="$(location BUILD.bazel.out)"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("off-mode project A BUILD missing %q:\n%s", want, body)
		}
	}
	// Assert on split-only *constructs* (a threaded flag line, the
	// tar command, the tar output entry) rather than bare tokens that
	// also appear in the template's explanatory comment.
	for _, unwanted := range []string{
		`--out-build="$$PKGTREE/BUILD.bazel"`,
		`tar -cf "$(location build-packages.tar)" -C "$$PKGTREE" .`,
		`"build-packages.tar",`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("off-mode project A BUILD unexpectedly contains split construct %q:\n%s", unwanted, body)
		}
	}
}
