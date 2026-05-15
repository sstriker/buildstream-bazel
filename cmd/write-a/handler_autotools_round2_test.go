package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriter_AutotoolsRound2_ProjectAConverterGenrule covers the
// load-bearing pivot: with round-2 (the default when --convert-element-trace is set) enabled, project A
// for a kind:autotools element renders a converter genrule whose
// srcs reference :<elem>_trace_load. That's the round-2 rendezvous
// wiring — the converter consumes the trace fileset the action-
// time trace_load rule produces (or zero-bytes-with-miss-marker
// when the AC misses).
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

	// A-side: the converter genrule consuming :<elem>_trace_load,
	// and the trace_load target itself emitted alongside.
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "auto_build"`,
		`load("//rules:traces.bzl", "trace_load")`,
		`name = "auto_trace_load"`,
		`":auto_trace_load"`,
		`"srckey.txt"`,
		`"//tools:convert-element-trace"`,
		`--trace-dir`,
		`--out-build`,
	} {
		if !strings.Contains(string(aBody), want) {
			t.Errorf("project A round-2 BUILD missing %q\n%s", want, aBody)
		}
	}
	for _, unwanted := range []string{
		`"@trace_auto//:trace"`,
		"BUILD_IN_PROJECT_B",
	} {
		if strings.Contains(string(aBody), unwanted) {
			t.Errorf("project A round-2 BUILD unexpectedly contains %q\n%s", unwanted, aBody)
		}
	}

	// srckey.txt must exist alongside the converter genrule —
	// the genrule's command reads it to derive the synthetic AC
	// key (well, indirectly via trace-lookup at load time —
	// but the file presence + per-element shape is the contract).
	if _, err := os.Stat(filepath.Join(outA, "elements/auto/srckey.txt")); err != nil {
		t.Errorf("srckey.txt not staged in project A: %v", err)
	}

	// srckey-patterns.txt is the per-element pattern surface
	// cmd/audit-narrowing reads to flag undercoverage drift.
	// For kind:autotools that's autotoolsSrckeyPatterns()'s
	// content-include rules, formatted in read-paths.txt syntax.
	patternsBody, err := os.ReadFile(filepath.Join(outA, "elements/auto/srckey-patterns.txt"))
	if err != nil {
		t.Errorf("srckey-patterns.txt not staged in project A: %v", err)
	}
	for _, want := range []string{
		"include configure.ac\n",
		"include **/*.h\n",
	} {
		if !strings.Contains(string(patternsBody), want) {
			t.Errorf("srckey-patterns.txt missing rule %q\n%s", want, patternsBody)
		}
	}

	// rules/traces.bzl renders the trace_load rule. tools/traces.json
	// is no longer emitted (the load-time `traces` module extension
	// was retired when the AC lookup moved to action time).
	tracesBzl, err := os.ReadFile(filepath.Join(outA, "rules/traces.bzl"))
	if err != nil {
		t.Fatalf("rules/traces.bzl missing: %v", err)
	}
	for _, want := range []string{
		"trace_load = rule",
		"_trace_load_impl",
		"ctx.actions.run",
		"--srckey",
	} {
		if !strings.Contains(string(tracesBzl), want) {
			t.Errorf("rules/traces.bzl missing %q\n%s", want, tracesBzl)
		}
	}
	if _, err := os.Stat(filepath.Join(outA, "tools/traces.json")); !os.IsNotExist(err) {
		t.Errorf("tools/traces.json should not be emitted; got err=%v", err)
	}

	// MODULE.bazel no longer declares the traces extension — the
	// trace_load rule is a regular rule, not an extension repo.
	modA, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{
		`use_extension("//rules:traces.bzl", "traces")`,
		`"trace_auto"`,
	} {
		if strings.Contains(string(modA), unwanted) {
			t.Errorf("project A MODULE.bazel unexpectedly contains legacy traces extension wiring %q\n%s", unwanted, modA)
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
		`//tools:convert-element-trace`,
	} {
		if strings.Contains(string(bBody), banned) {
			t.Errorf("project B round-2 BUILD unexpectedly contains %q\n%s", banned, bBody)
		}
	}
}

// TestWriter_AutotoolsRound2_MultiPlatform_ProjectB: kind:autotools
// shares the same project-B per-platform install fan-out shape
// pipelineHandler kinds use (kind:make / manual / script /
// makemaker / modulebuild). With --platforms-json set + round-2
// active, an autotools element renders N install genrules in
// project B + a top-level select()-filegroup at install_tree.tar
// — same shape TestWriter_PipelineKindsRound2_MultiPlatform_
// ProjectB asserts for kind:make, just at the autotoolsHandler
// dispatch site.
func TestWriter_AutotoolsRound2_MultiPlatform_ProjectB(t *testing.T) {
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
	if err := os.WriteFile(bst, []byte("kind: autotools\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"convert-element-trace-fake", "build-tracer-fake", "trace-publish-fake", "trace-lookup-fake", "fold-element-fake"} {
		if err := os.WriteFile(filepath.Join(tmp, name),
			[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := traceConfig
	traceConfig.convertBin = filepath.Join(tmp, "convert-element-trace-fake")
	traceConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	traceConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	traceConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
	traceConfig.foldBin = filepath.Join(tmp, "fold-element-fake")
	traceConfig.round2Enabled = true
	traceConfig.platforms = []tracePlatform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}},
	}
	if err := resolvePlatformSelectKeys(traceConfig.platforms); err != nil {
		t.Fatalf("resolvePlatformSelectKeys: %v", err)
	}
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
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(bBody)

	// N per-platform install genrules + top-level filegroup.
	for _, want := range []string{
		`name = "auto_install_linux_x86_64"`,
		`name = "auto_install_darwin_arm64"`,
		`"linux_x86_64/install_tree.tar"`,
		`"darwin_arm64/install_tree.tar"`,
		`"linux_x86_64/trace.log"`,
		`"darwin_arm64/trace.log"`,
		`"linux_x86_64/generated-headers.txt"`,
		`"darwin_arm64/generated-headers.txt"`,
		// exec_compatible_with renders sorted (matches the
		// projecta/render.go precedent): @platforms//cpu:
		// precedes @platforms//os: alphabetically. Phase 3's
		// buildtools-canonical formatter wraps the 2-element
		// list across lines; assert on per-element substrings.
		`"@platforms//cpu:x86_64"`,
		`"@platforms//os:linux"`,
		`"@platforms//cpu:arm64"`,
		`"@platforms//os:darwin"`,
		`--platform="linux_x86_64"`,
		`--platform="darwin_arm64"`,
		`name = "install_tree.tar"`,
		`["linux_x86_64/install_tree.tar"]`,
		`["darwin_arm64/install_tree.tar"]`,
		`"//conditions:default": [],`,
		`$(location linux_x86_64/generated-headers.txt)`,
		`$(location darwin_arm64/generated-headers.txt)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("multi-platform autotools project B missing %q\n%s", want, got)
		}
	}

	// Legacy single-platform genrule name must NOT appear.
	if strings.Contains(got, `name = "auto_install"`) {
		t.Errorf("multi-platform autotools project B unexpectedly contains legacy 'auto_install' name (no platform suffix)\n%s", got)
	}
}
