package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriter_MakeRound2_ProjectAConverterGenrule covers kind:make
// joining the trace-driven round-2 path: with the trace-driven
// binaries supplied to write-a, project A hosts a per-element
// converter genrule consuming @trace_<elem>//:trace, and project
// B hosts the coarse install genrule with the inline
// trace-publish step. Same architecture as kind:autotools — this
// test is the kind:make-specific assertion.
func TestWriter_MakeRound2_ProjectAConverterGenrule(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Makefile"),
		[]byte("all:\n\tcc -o greet greet.c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "greet.c"),
		[]byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "mk.bst")
	bstBody := "kind: make\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Marker-shaped fakes for the four trace-driven binaries.
	for _, name := range []string{
		"convert-element-trace-fake",
		"build-tracer-fake",
		"trace-publish-fake",
		"trace-lookup-fake",
	} {
		if err := os.WriteFile(filepath.Join(tmp, name),
			[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	traceConfig.convertBin = filepath.Join(tmp, "convert-element-trace-fake")
	traceConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	traceConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	traceConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
	traceConfig.round2Enabled = true
	t.Cleanup(func() {
		traceConfig.convertBin = ""
		traceConfig.tracerBin = ""
		traceConfig.publishBin = ""
		traceConfig.lookupBin = ""
		traceConfig.round2Enabled = false
	})

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	outB := filepath.Join(tmp, "B")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}

	// A-side: per-element converter genrule consuming @trace_mk//:trace.
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/mk/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "mk_build"`,
		`"@trace_mk//:trace"`,
		`"srckey.txt"`,
		`"//tools:convert-element-trace"`,
		`--trace-dir`,
		`--out-build`,
		`kind:make round-2`,
	} {
		if !strings.Contains(string(aBody), want) {
			t.Errorf("project A round-2 BUILD missing %q\n%s", want, aBody)
		}
	}
	// kind:make's pipelineHandler.RenderA must NOT have emitted
	// the install genrule into A under round-2 — that genrule
	// moved to B. Sanity-check it isn't here.
	if strings.Contains(string(aBody), `name = "mk_install"`) {
		t.Errorf("project A round-2 BUILD unexpectedly contains the install genrule (mk_install); should have moved to B")
	}

	if _, err := os.Stat(filepath.Join(outA, "elements/mk/srckey.txt")); err != nil {
		t.Errorf("srckey.txt not staged in project A: %v", err)
	}

	// rules/traces.bzl + tools/traces.json render in both projects.
	for _, p := range []string{
		"rules/traces.bzl",
		"tools/traces.json",
	} {
		if _, err := os.Stat(filepath.Join(outA, p)); err != nil {
			t.Errorf("project A missing %s: %v", p, err)
		}
		if _, err := os.Stat(filepath.Join(outB, p)); err != nil {
			t.Errorf("project B missing %s: %v", p, err)
		}
	}

	// MODULE.bazel declares the traces extension + the per-element repo.
	modA, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`use_extension("//rules:traces.bzl", "traces")`,
		`"trace_mk"`,
	} {
		if !strings.Contains(string(modA), want) {
			t.Errorf("project A MODULE.bazel missing %q\n%s", want, modA)
		}
	}

	// B-side: coarse install genrule + trace-publish inline; no
	// converter (that moved to A).
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/mk/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "mk_install"`,
		`"install_tree.tar"`,
		`"trace.log"`,
		`"make-db.txt"`,
		`"//tools:build-tracer"`,
		`"//tools:trace-publish"`,
		`$(location //tools:trace-publish)`,
		`CAS_GRPC_ADDR`,
		`--srckey=`,
	} {
		if !strings.Contains(string(bBody), want) {
			t.Errorf("project B round-2 BUILD missing %q\n%s", want, bBody)
		}
	}
	for _, banned := range []string{
		`"BUILD.bazel.out"`,
		`"install-mapping.json"`,
		`//tools:convert-element-trace`,
	} {
		if strings.Contains(string(bBody), banned) {
			t.Errorf("project B round-2 BUILD unexpectedly contains %q\n%s", banned, bBody)
		}
	}
}

// TestWriter_MakeWithoutRound2_StillRendersInstallInA covers the
// backwards-compat path: when the trace-driven binaries aren't
// supplied (or --autotools-round1 is passed), kind:make renders
// with its legacy shape — install genrule lives in project A,
// project B is the placeholder. New opt-in field on
// pipelineHandler must not affect this case.
func TestWriter_MakeWithoutRound2_StillRendersInstallInA(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Makefile"),
		[]byte("all:\n\tcc -o greet greet.c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "greet.c"),
		[]byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "mk.bst")
	bstBody := "kind: make\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// No trace-driven binaries → no opt-in → legacy shape.
	// Snapshot + restore the full traceConfig struct via
	// t.Cleanup so this test isn't order-dependent if a future
	// test expects non-zero state.
	prev := traceConfig
	traceConfig.convertBin = ""
	traceConfig.tracerBin = ""
	traceConfig.publishBin = ""
	traceConfig.lookupBin = ""
	traceConfig.round2Enabled = false
	t.Cleanup(func() { traceConfig = prev })

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	outB := filepath.Join(tmp, "B")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}

	// A-side: legacy install genrule shape (install_tree.tar).
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/mk/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "mk_install"`,
		`"install_tree.tar"`,
	} {
		if !strings.Contains(string(aBody), want) {
			t.Errorf("project A legacy BUILD missing %q\n%s", want, aBody)
		}
	}
	// No converter-genrule shape leaking through.
	for _, banned := range []string{
		`"@trace_mk//:trace"`,
		`name = "mk_build"`,
	} {
		if strings.Contains(string(aBody), banned) {
			t.Errorf("project A legacy BUILD unexpectedly contains round-2 marker %q", banned)
		}
	}

	// B-side: placeholder.
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/mk/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bBody), "BUILD_NOT_YET_STAGED") {
		t.Errorf("project B legacy BUILD should be the placeholder; got:\n%s", bBody)
	}
}
