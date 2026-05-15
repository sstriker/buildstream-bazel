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

// TestMesonElement_Round2Fallback verifies the per-element BUILD
// shape when --meson-round2-fallback is configured: A's converter
// genrule threads --unsupported-target-fallback=true into
// convert-element-meson AND pulls @trace_<elem>//:trace into srcs
// (the load-time AC lookup; trace-driven convergence research
// follow-on teaches convert-element-meson to consume the trace).
// B's install genrule replaces the placeholder.
func TestMesonElement_Round2Fallback(t *testing.T) {
	tmp := t.TempDir()
	prev := mesonConfig
	mesonBin := filepath.Join(tmp, "convert-element-meson-fake")
	if err := os.WriteFile(mesonBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mesonConfig.convertBin = mesonBin
	mesonConfig.round2FallbackEnabled = true
	// Trace plumbing needs to be populated for collectTraces /
	// trace_repo wiring. Use mock paths; the test doesn't run
	// Bazel.
	prevTrace := traceConfig
	tracerBin := filepath.Join(tmp, "build-tracer-fake")
	publishBin := filepath.Join(tmp, "trace-publish-fake")
	lookupBin := filepath.Join(tmp, "trace-lookup-fake")
	for _, p := range []string{tracerBin, publishBin, lookupBin} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	traceConfig.tracerBin = tracerBin
	traceConfig.publishBin = publishBin
	traceConfig.lookupBin = lookupBin
	defer func() {
		mesonConfig = prev
		traceConfig = prevTrace
	}()

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

	// A-side: converter genrule threads the fallback flag AND
	// pulls the trace label into srcs. The trace module wiring
	// (rules/traces.bzl, tools/traces.json) is rendered too.
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`--unsupported-target-fallback=true`,
		`"@trace_elem//:trace"`,
		`//tools:convert-element-meson`,
	} {
		if !strings.Contains(string(aBody), marker) {
			t.Errorf("A-side BUILD missing marker %q\n%s", marker, aBody)
		}
	}
	for _, path := range []string{
		"rules/traces.bzl",
		"tools/traces.json",
		"tools/build-tracer",
		"tools/trace-publish",
		"tools/trace-lookup",
	} {
		if _, err := os.Stat(filepath.Join(outA, path)); err != nil {
			t.Errorf("project A missing %s: %v", path, err)
		}
	}

	// B-side: real install genrule replaces the placeholder.
	outB := filepath.Join(tmp, "project-B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`name = "elem_install"`,
		`"install_tree.tar"`,
		`"trace.log"`,
		`"//tools:build-tracer"`,
		`"//tools:trace-publish"`,
		`meson setup "$$BUILD_ROOT" "$$SRC_DIR" --prefix=/ --libdir=lib`,
		`ninja -C "$$BUILD_ROOT"`,
		`DESTDIR="$$INSTALL_ROOT" meson install -C "$$BUILD_ROOT"`,
		`CAS_GRPC_ADDR`,
		`--srckey=`,
	} {
		if !strings.Contains(string(bBody), marker) {
			t.Errorf("B-side BUILD missing marker %q\n%s", marker, bBody)
		}
	}
	if strings.Contains(string(bBody), "BUILD_NOT_YET_STAGED") {
		t.Errorf("B-side still has placeholder; should have install genrule:\n%s", bBody)
	}
	// srckey.txt is staged in B (trace-publish reads it).
	if _, err := os.Stat(filepath.Join(outB, "elements/elem/srckey.txt")); err != nil {
		t.Errorf("project B missing srckey.txt: %v", err)
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
