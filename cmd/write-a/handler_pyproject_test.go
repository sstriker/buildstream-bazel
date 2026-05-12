package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePyprojectBst = `kind: pyproject

sources:
- kind: local
  path: src
`

// TestPyprojectElement_PipelineFallback verifies the historical
// pipeline-shape render is preserved when --convert-element-
// pyproject isn't supplied (pyprojectConfig.convertBin is
// empty). Project A emits the coarse install_tree.tar genrule;
// project B is a placeholder.
func TestPyprojectElement_PipelineFallback(t *testing.T) {
	prev := pyprojectConfig.convertBin
	pyprojectConfig.convertBin = ""
	defer func() { pyprojectConfig.convertBin = prev }()

	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "pyproject.toml"),
		[]byte(`[build-system]
requires = ["setuptools>=64"]
build-backend = "setuptools.build_meta"

[project]
name = "demo"
version = "0.1.0"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bstPath, []byte(samplePyprojectBst), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := fakeConvertBin(t, tmp)

	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	// Pipeline shape: invokes python -m build / python -m pip
	// install via the variables: defaults; output is install_tree.tar.
	for _, marker := range []string{
		"python",
		"build",
		"install_tree.tar",
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("pipeline-fallback BUILD missing marker %q\n%s", marker, got)
		}
	}
	// Native-path-only markers must be absent.
	for _, dropped := range []string{
		"//tools:convert-element-pyproject",
	} {
		if strings.Contains(got, dropped) {
			t.Errorf("pipeline-fallback BUILD unexpectedly contains %q\n%s", dropped, got)
		}
	}
}

// TestPyprojectElement_NativeRender verifies the per-element
// BUILD.bazel shape when --convert-element-pyproject is
// configured: a converter genrule with the
// //tools:convert-element-pyproject invocation, BUILD.bazel.out
// declared as the only out, and the convert-element-pyproject
// binary staged into project A's tools/.
func TestPyprojectElement_NativeRender(t *testing.T) {
	tmp := t.TempDir()
	prev := pyprojectConfig.convertBin
	pyprojectBin := filepath.Join(tmp, "convert-element-pyproject-fake")
	if err := os.WriteFile(pyprojectBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pyprojectConfig.convertBin = pyprojectBin
	defer func() { pyprojectConfig.convertBin = prev }()

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "pyproject.toml"),
		[]byte(`[build-system]
requires = ["setuptools>=64"]
build-backend = "setuptools.build_meta"

[project]
name = "demo"
version = "0.1.0"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bstPath, []byte(samplePyprojectBst), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := fakeConvertBin(t, tmp)

	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outA, "tools/convert-element-pyproject")); err != nil {
		t.Errorf("convert-element-pyproject not staged: %v", err)
	}
	toolsBuild, err := os.ReadFile(filepath.Join(outA, "tools/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toolsBuild), `"convert-element-pyproject"`) {
		t.Errorf("tools/BUILD.bazel missing convert-element-pyproject export:\n%s", toolsBuild)
	}

	body, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, marker := range []string{
		`tools = ["//tools:convert-element-pyproject"]`,
		`$(location //tools:convert-element-pyproject)`,
		`"BUILD.bazel.out"`,
		`name = "elem_converted"`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("native-render BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// Pipeline-fallback markers must be absent in native mode.
	for _, dropped := range []string{
		"install_tree.tar",
	} {
		if strings.Contains(got, dropped) {
			t.Errorf("native-render BUILD unexpectedly contains %q\n%s", dropped, got)
		}
	}

	outB := filepath.Join(tmp, "project-B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	bModule, err := os.ReadFile(filepath.Join(outB, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bModule), `bazel_dep(name = "rules_python"`) {
		t.Errorf("project B MODULE.bazel missing rules_python bazel_dep (expected because graph has a kind:pyproject element with native render enabled):\n%s", bModule)
	}
}

// TestPyprojectElement_DirectoryForcesPipelineShape verifies
// that an element whose source has Directory != "" routes to
// the pipeline-shape coarse install genrule even with
// --convert-element-pyproject set. The native genrule's
// shadow-merge strips up to `sources/` from each input path
// and invokes the converter with --source-root=$SHADOW, so a
// pyproject.toml that's staged at sources/<Directory>/
// pyproject.toml wouldn't be found. Pipeline shape handles
// Directory natively via the existing pipeline-handler
// staging.
func TestPyprojectElement_DirectoryForcesPipelineShape(t *testing.T) {
	tmp := t.TempDir()
	prev := pyprojectConfig.convertBin
	pyprojectBin := filepath.Join(tmp, "convert-element-pyproject-fake")
	if err := os.WriteFile(pyprojectBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pyprojectConfig.convertBin = pyprojectBin
	defer func() { pyprojectConfig.convertBin = prev }()
	// Reset the structural-fallback cache between tests so the
	// run sees this element fresh (test order is otherwise non-
	// deterministic via go test's randomization).
	prevCache := pyprojectStructuralFallback
	pyprojectStructuralFallback = map[string]bool{}
	defer func() { pyprojectStructuralFallback = prevCache }()

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "pyproject.toml"),
		[]byte(`[build-system]
requires = ["setuptools>=64"]
build-backend = "setuptools.build_meta"

[project]
name = "demo"
version = "0.1.0"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "elem.bst")
	// kind:local source with Directory: stages under
	// sources/subdir/pyproject.toml; native render's
	// --source-root=$SHADOW wouldn't find pyproject.toml.
	if err := os.WriteFile(bstPath, []byte(`kind: pyproject

sources:
- kind: local
  path: src
  directory: subdir
`), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := fakeConvertBin(t, tmp)

	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "install_tree.tar") {
		t.Errorf("Directory-set element should have routed to pipeline shape (install_tree.tar) regardless of --convert-element-pyproject, but got native render:\n%s", got)
	}
	if strings.Contains(got, "//tools:convert-element-pyproject") {
		t.Errorf("Directory-set element unexpectedly rendered the native genrule:\n%s", got)
	}
}
