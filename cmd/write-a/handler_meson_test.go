package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleMesonBst = `kind: meson

sources:
- kind: local
  path: src
`

// TestMesonElement_PipelineFallback verifies the historical
// pipeline-shape render is preserved when --convert-element-meson
// isn't supplied (mesonConfig.convertBin is empty). Project A
// emits the coarse install_tree.tar genrule; project B is a
// placeholder.
func TestMesonElement_PipelineFallback(t *testing.T) {
	prev := mesonConfig.convertBin
	mesonConfig.convertBin = ""
	defer func() { mesonConfig.convertBin = prev }()

	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "meson.build"),
		[]byte("project('p', 'c')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bstPath, []byte(sampleMesonBst), 0o644); err != nil {
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
	// Pipeline shape: an install genrule wrapping `meson` /
	// `ninja` invocations, output is install_tree.tar.
	for _, marker := range []string{
		"meson",
		"ninja",
		"install_tree.tar",
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("pipeline-fallback BUILD missing marker %q\n%s", marker, got)
		}
	}
	// Native-path-only markers must be absent.
	for _, dropped := range []string{
		"//tools:convert-element-meson",
		"pkg-config-bundle.tar",
	} {
		if strings.Contains(got, dropped) {
			t.Errorf("pipeline-fallback BUILD unexpectedly contains %q\n%s", dropped, got)
		}
	}
}

// TestMesonElement_NativeRender verifies the per-element BUILD.bazel
// shape when --convert-element-meson is configured: a converter
// genrule with the expected outputs + the //tools:convert-element-meson
// invocation, and the convert-element-meson binary is staged into
// project A's tools/.
func TestMesonElement_NativeRender(t *testing.T) {
	tmp := t.TempDir()
	prev := mesonConfig.convertBin
	// Stage a fake convert-element-meson "binary" — the writer
	// only stat()s and copies it.
	mesonBin := filepath.Join(tmp, "convert-element-meson-fake")
	if err := os.WriteFile(mesonBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mesonConfig.convertBin = mesonBin
	defer func() { mesonConfig.convertBin = prev }()

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "meson.build"),
		[]byte("project('p', 'c')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bstPath, []byte(sampleMesonBst), 0o644); err != nil {
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

	// convert-element-meson staged + listed in tools/BUILD.bazel.
	if _, err := os.Stat(filepath.Join(outA, "tools/convert-element-meson")); err != nil {
		t.Errorf("convert-element-meson not staged: %v", err)
	}
	toolsBuild, err := os.ReadFile(filepath.Join(outA, "tools/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toolsBuild), `"convert-element-meson"`) {
		t.Errorf("tools/BUILD.bazel missing convert-element-meson export:\n%s", toolsBuild)
	}

	body, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, marker := range []string{
		`tools = ["//tools:convert-element-meson"]`,
		`$(location //tools:convert-element-meson)`,
		`"BUILD.bazel.out"`,
		`"pkg-config-bundle.tar"`,
		`name = "elem_converted"`,
		`name = "elem_real"`,
		`for src in $(SRCS)`,
		`rel="$${src##*sources/}"`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("native-render BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// Pipeline-fallback markers must be absent.
	for _, dropped := range []string{
		"install_tree.tar",
	} {
		if strings.Contains(got, dropped) {
			t.Errorf("native-render BUILD unexpectedly contains %q\n%s", dropped, got)
		}
	}

	// Project B's per-element package: sources staged + placeholder
	// BUILD.bazel emitted.
	outB := filepath.Join(tmp, "project-B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bBody), "BUILD_NOT_YET_STAGED") {
		t.Errorf("project B elem BUILD missing placeholder:\n%s", bBody)
	}
	if _, err := os.Stat(filepath.Join(outB, "elements/elem/meson.build")); err != nil {
		t.Errorf("project B elem source not staged: %v", err)
	}
}
