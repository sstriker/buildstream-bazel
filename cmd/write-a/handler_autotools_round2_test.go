package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriter_AutotoolsRound2_ProjectAConverterGenrule covers the
// load-bearing pivot: with --autotools-round2 enabled, project A
// for a kind:autotools element renders a converter genrule whose
// srcs reference @trace_<elem>//:trace. That's the round-2
// rendezvous wiring — the converter consumes the trace fileset
// the _trace_repo rule produces (or doesn't) at load time.
func TestWriter_AutotoolsRound2_ProjectAConverterGenrule(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "configure"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Makefile.in"),
		[]byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "auto.bst")
	bstBody := "kind: autotools\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Marker-shaped fakes.
	for _, name := range []string{
		"convert-element-autotools-fake",
		"build-tracer-fake",
		"trace-publish-fake",
		"trace-lookup-fake",
	} {
		if err := os.WriteFile(filepath.Join(tmp, name),
			[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	autotoolsConfig.convertBin = filepath.Join(tmp, "convert-element-autotools-fake")
	autotoolsConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	autotoolsConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	autotoolsConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
	autotoolsConfig.round2Enabled = true
	t.Cleanup(func() {
		autotoolsConfig.convertBin = ""
		autotoolsConfig.tracerBin = ""
		autotoolsConfig.publishBin = ""
		autotoolsConfig.lookupBin = ""
		autotoolsConfig.round2Enabled = false
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

	// A-side: the converter genrule consuming @trace_<elem>//:trace.
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "auto_build"`,
		`"@trace_auto//:trace"`,
		`"srckey.txt"`,
		`"//tools:convert-element-autotools"`,
		`--trace-dir`,
		`--out-build`,
	} {
		if !strings.Contains(string(aBody), want) {
			t.Errorf("project A round-2 BUILD missing %q\n%s", want, aBody)
		}
	}
	if strings.Contains(string(aBody), "BUILD_IN_PROJECT_B") {
		t.Errorf("project A round-2 BUILD should NOT be the round-1 marker:\n%s", aBody)
	}

	// srckey.txt must exist alongside the converter genrule —
	// the genrule's command reads it to derive the synthetic AC
	// key (well, indirectly via trace-lookup at load time —
	// but the file presence + per-element shape is the contract).
	if _, err := os.Stat(filepath.Join(outA, "elements/auto/srckey.txt")); err != nil {
		t.Errorf("srckey.txt not staged in project A: %v", err)
	}

	// rules/traces.bzl + tools/traces.json must both render.
	tracesBzl, err := os.ReadFile(filepath.Join(outA, "rules/traces.bzl"))
	if err != nil {
		t.Fatalf("rules/traces.bzl missing: %v", err)
	}
	for _, want := range []string{
		"_trace_repo",
		"TRACE_LOOKUP_BIN",
		"CAS_GRPC_ADDR",
		"CAS_FUSE_MOUNT",
		"trace-lookup",
	} {
		if !strings.Contains(string(tracesBzl), want) {
			t.Errorf("rules/traces.bzl missing %q\n%s", want, tracesBzl)
		}
	}
	tracesJSON, err := os.ReadFile(filepath.Join(outA, "tools/traces.json"))
	if err != nil {
		t.Fatalf("tools/traces.json missing: %v", err)
	}
	if !strings.Contains(string(tracesJSON), `"key": "auto"`) {
		t.Errorf("tools/traces.json missing element entry:\n%s", tracesJSON)
	}

	// MODULE.bazel must declare the traces extension + use_repo.
	modA, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`use_extension("//rules:traces.bzl", "traces")`,
		`"trace_auto"`,
	} {
		if !strings.Contains(string(modA), want) {
			t.Errorf("project A MODULE.bazel missing %q\n%s", want, modA)
		}
	}

	// trace-publish + trace-lookup staged in project A's tools/.
	for _, want := range []string{
		"tools/trace-publish",
		"tools/trace-lookup",
	} {
		if _, err := os.Stat(filepath.Join(outA, want)); err != nil {
			t.Errorf("project A missing staged tool %s: %v", want, err)
		}
	}

	// B-side: coarse install genrule lives here, with the
	// trace-publish step inline (no converter inline).
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "auto_install"`,
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
		// Round-2's pass-3 must NOT run the converter inline.
		`"BUILD.bazel.out"`,
		`"install-mapping.json"`,
		`//tools:convert-element-autotools`,
	} {
		if strings.Contains(string(bBody), banned) {
			t.Errorf("project B round-2 BUILD unexpectedly contains %q\n%s", banned, bBody)
		}
	}
}
