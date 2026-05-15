package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmakeDepBundleLabels_TraceDrivenDep covers the cross-element
// configure-step bootstrap rendezvous (see
// docs/design/cross-element-config-rendezvous.md): a kind:cmake
// element with a trace-driven (kind:autotools) dep gets the dep's
// :<dep>_trace_load target staged via cmakeDepBundleLabels so the
// AC-published config bundle can flow into the consumer's
// $PREFIX. Pre-this-PR, the cmake filter dropped non-cmake deps
// silently; this test pins the new behaviour.
func TestCmakeDepBundleLabels_TraceDrivenDep(t *testing.T) {
	tmp := t.TempDir()

	// Stage two sibling elements: a kind:autotools "auto-dep"
	// and a kind:cmake "consumer" that depends on it.
	autoSrc := filepath.Join(tmp, "auto-src")
	if err := os.MkdirAll(autoSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(autoSrc, "configure"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(autoSrc, "Makefile.in"), []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "auto-dep.bst"),
		[]byte("kind: autotools\nsources:\n- kind: local\n  path: "+autoSrc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	consumerSrc := filepath.Join(tmp, "consumer-src")
	if err := os.MkdirAll(consumerSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumerSrc, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(c C)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "consumer.bst"),
		[]byte("kind: cmake\ndepends:\n- auto-dep.bst\nsources:\n- kind: local\n  path: "+consumerSrc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Activate the trace-driven autotools path so
	// traceDrivenSrckeyPatternsForKind("autotools") returns
	// non-nil, which is what cmakeDepBundleLabels keys off.
	for _, name := range []string{"convert-element-trace-fake", "build-tracer-fake", "trace-publish-fake", "trace-lookup-fake"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := traceConfig
	traceConfig.convertBin = filepath.Join(tmp, "convert-element-trace-fake")
	traceConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	traceConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	traceConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
	traceConfig.round2Enabled = true
	t.Cleanup(func() { traceConfig = prev })

	g, err := loadGraph([]string{filepath.Join(tmp, "consumer.bst"), filepath.Join(tmp, "auto-dep.bst")}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	// Consumer's BUILD must reference the dep's :<dep>_trace_load
	// (the bundle source) in its converter genrule srcs.
	consumerBody, err := os.ReadFile(filepath.Join(outA, "elements/consumer/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"//elements/auto-dep:auto-dep_trace_load"`,
		// imports.json must be staged too (consumer has at least
		// one bundle-bearing dep).
		`"imports.json"`,
	} {
		if !strings.Contains(string(consumerBody), want) {
			t.Errorf("consumer BUILD missing marker %q\n%s", want, consumerBody)
		}
	}
	// Legacy kind=cmake filter would have produced no bundle ref.
	// Confirm we're not emitting the cmake-bundle filegroup for
	// the autotools dep.
	if strings.Contains(string(consumerBody), `"//elements/auto-dep:cmake_config_bundle"`) {
		t.Errorf("consumer BUILD unexpectedly references kind:cmake bundle filegroup for kind:autotools dep")
	}

	// auto-dep's BUILD must declare the trace_load target with
	// expect_config_bundle=True so the bundle output exists.
	depBody, err := os.ReadFile(filepath.Join(outA, "elements/auto-dep/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "auto-dep_trace_load"`,
		`expect_config_bundle = True`,
	} {
		if !strings.Contains(string(depBody), want) {
			t.Errorf("dep BUILD missing trace_load marker %q\n%s", want, depBody)
		}
	}
}
