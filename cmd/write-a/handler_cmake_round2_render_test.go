package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriter_CmakeRound2Fallback_RenderShape covers the
// end-to-end write-a render for kind:cmake when
// --cmake-round2-fallback is enabled: A's converter genrule
// threads --unsupported-execute-process-fallback=true; B's
// per-element BUILD emits a real install genrule (cmake +
// ninja + install + tar + trace-publish) instead of the
// placeholder; build-tracer + trace-publish stage into both
// projects' tools/.
func TestWriter_CmakeRound2Fallback_RenderShape(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(demo C)\nadd_library(thelib STATIC src/lib.c)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "src", "lib.c"),
		[]byte("int the_function(void){return 42;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "demo.bst")
	bstBody := "kind: cmake\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Marker-shaped fakes for the round-2 binaries.
	for _, name := range []string{"build-tracer-fake", "trace-publish-fake", "trace-lookup-fake"} {
		if err := os.WriteFile(filepath.Join(tmp, name),
			[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Snapshot + restore cmakeConfig / traceConfig so the
	// test isn't order-dependent. The kind:cmake round-2
	// fallback uses the same kind-agnostic round-2 binaries
	// autotools does (build-tracer / trace-publish /
	// trace-lookup); they live on traceConfig (the shared
	// resolution target). Explicitly clear the autotools-
	// round-2-specific fields so stageAutotoolsTools doesn't
	// see leftover state from a prior test (notably
	// .round2Enabled, which would alter staging assertions
	// below).
	prevC := cmakeConfig
	prevA := traceConfig
	traceConfig.convertBin = ""
	traceConfig.lookupBin = ""
	traceConfig.round2Enabled = false
	traceConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	traceConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	traceConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
	cmakeConfig.round2FallbackEnabled = true
	t.Cleanup(func() {
		cmakeConfig = prevC
		traceConfig = prevA
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

	// A-side: converter genrule threads the fallback flag AND
	// pulls :demo_trace_load into srcs (the action-time AC lookup;
	// trace-driven convergence research follow-on teaches
	// convert-element-cmake to consume the trace bytes).
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--unsupported-execute-process-fallback=true",
		`load("//rules:traces.bzl", "trace_load")`,
		`name = "demo_trace_load"`,
		`expect_make_db = False`,
		`":demo_trace_load"`,
	} {
		if !strings.Contains(string(aBody), want) {
			t.Errorf("project A BUILD missing %q\n%s", want, aBody)
		}
	}
	if strings.Contains(string(aBody), `"@trace_demo//:trace"`) {
		t.Errorf("project A BUILD unexpectedly contains legacy @trace_*//:trace label:\n%s", aBody)
	}

	// rules/traces.bzl renders in both projects. tools/traces.json
	// is no longer emitted (the load-time extension was retired).
	for _, project := range []string{outA, outB} {
		if _, err := os.Stat(filepath.Join(project, "rules/traces.bzl")); err != nil {
			t.Errorf("%s missing rules/traces.bzl: %v", project, err)
		}
		if _, err := os.Stat(filepath.Join(project, "tools/traces.json")); !os.IsNotExist(err) {
			t.Errorf("%s unexpectedly emitted tools/traces.json (legacy load-time wiring)", project)
		}
	}

	// MODULE.bazel must NOT declare the legacy traces extension.
	modA, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{
		`use_extension("//rules:traces.bzl", "traces")`,
		`"trace_demo"`,
	} {
		if strings.Contains(string(modA), unwanted) {
			t.Errorf("project A MODULE.bazel unexpectedly contains legacy traces extension wiring %q\n%s", unwanted, modA)
		}
	}

	// B-side: real install genrule (no placeholder).
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "demo_trace_build"`,
		`tags = ["trace_build"]`,
		`"install_tree.tar"`,
		`"trace.log"`,
		`"//tools:build-tracer"`,
		`"//tools:trace-publish"`,
		`cmake -B`,
		`cmake --build`,
		`cmake --install`,
		`CAS_GRPC_ADDR`,
		`--srckey=`,
	} {
		if !strings.Contains(string(bBody), want) {
			t.Errorf("project B round-2 install BUILD missing %q\n%s", want, bBody)
		}
	}
	if strings.Contains(string(bBody), "BUILD_NOT_YET_STAGED") {
		t.Errorf("project B round-2 should NOT emit the placeholder when fallback is enabled; got:\n%s", bBody)
	}

	// srckey.txt is staged in B (trace-publish reads it).
	if _, err := os.Stat(filepath.Join(outB, "elements/demo/srckey.txt")); err != nil {
		t.Errorf("srckey.txt not staged in project B: %v", err)
	}

	// build-tracer + trace-publish + trace-lookup stage into
	// both projects' tools/. Wiring all three at once means
	// the trace-driven convergence research follow-on
	// (teaching convert-element-cmake to consume @trace_<elem>//:trace
	// to refine refusals into fine cc rules) is purely a
	// converter-side change — no further write-a work.
	for _, project := range []string{outA, outB} {
		for _, tool := range []string{"build-tracer", "trace-publish", "trace-lookup"} {
			if _, err := os.Stat(filepath.Join(project, "tools", tool)); err != nil {
				t.Errorf("%s missing tools/%s: %v", project, tool, err)
			}
		}
	}
}

// TestWriter_CmakeRound2Fallback_OffByDefault asserts the
// existing kind:cmake render shape stays unchanged when the
// fallback flag is off — guards against the new flag
// accidentally short-circuiting the legacy path.
func TestWriter_CmakeRound2Fallback_OffByDefault(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(demo C)\nadd_library(thelib STATIC src/lib.c)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "src", "lib.c"),
		[]byte("int x(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "demo.bst")
	if err := os.WriteFile(bst,
		[]byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := cmakeConfig
	cmakeConfig.round2FallbackEnabled = false
	t.Cleanup(func() { cmakeConfig = prev })

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

	aBody, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(aBody), "--unsupported-execute-process-fallback=true") {
		t.Errorf("project A BUILD should NOT thread fallback flag when off; got:\n%s", aBody)
	}

	bBody, err := os.ReadFile(filepath.Join(outB, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bBody), "BUILD_NOT_YET_STAGED") {
		t.Errorf("project B should retain the placeholder when fallback is off; got:\n%s", bBody)
	}
	for _, banned := range []string{
		`name = "demo_install"`,
		`name = "demo_trace_build"`,
	} {
		if strings.Contains(string(bBody), banned) {
			t.Errorf("project B should NOT emit install genrule when fallback is off; got %q in:\n%s", banned, bBody)
		}
	}
}

// TestWriter_CmakeRound2Fallback_MultiPlatform_ProjectB: with
// --platforms-json set + --cmake-round2-fallback enabled,
// project B's per-element render fans out to N install
// genrules (one per platform) + a top-level select()-filegroup
// at :install_tree.tar. Same shape pipelineHandler kinds and
// kind:autotools got in #114 / #115, just at the cmake handler
// dispatch site.
//
// Each per-platform install genrule:
//   - Names "<elem>_install_<platform>"
//   - Outputs land under <platform>/install_tree.tar + <platform>/trace.log
//   - exec_compatible_with carries the platform's constraint set
//     (sorted for byte stability)
//   - trace-publish bakes --platform=<plat> literally
//
// Downstream //elements/<dep>:install_tree.tar references stay
// valid via the top-level filegroup; out-of-arm builds resolve
// to "//conditions:default": [].
func TestWriter_CmakeRound2Fallback_MultiPlatform_ProjectB(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(demo C)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "demo.bst")
	if err := os.WriteFile(bst,
		[]byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --platforms-json hard-requires the trace-driven round-2
	// path (main.go's platform gating rejects --platforms-json
	// without round-2); mirror that flag combination here.
	// convert-element-trace-fake unlocks round2Enabled =
	// true, the same shape production CLI lands.
	for _, name := range []string{"convert-element-trace-fake", "build-tracer-fake", "trace-publish-fake", "trace-lookup-fake", "fold-element-fake"} {
		if err := os.WriteFile(filepath.Join(tmp, name),
			[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prevC := cmakeConfig
	prevA := traceConfig
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
	cmakeConfig.round2FallbackEnabled = true
	t.Cleanup(func() {
		cmakeConfig = prevC
		traceConfig = prevA
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
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(bBody)

	for _, want := range []string{
		`name = "demo_trace_build_linux_x86_64"`,
		`name = "demo_trace_build_darwin_arm64"`,
		`tags = ["trace_build"]`,
		`"linux_x86_64/install_tree.tar"`,
		`"darwin_arm64/install_tree.tar"`,
		`"linux_x86_64/trace.log"`,
		`"darwin_arm64/trace.log"`,
		// exec_compatible_with sorted (@platforms//cpu:* precedes
		// @platforms//os:*). Phase 3's buildtools-canonical
		// formatter wraps 2-element lists across lines; assert
		// on per-element substrings.
		`"@platforms//cpu:x86_64"`,
		`"@platforms//os:linux"`,
		`"@platforms//cpu:arm64"`,
		`"@platforms//os:darwin"`,
		`--platform="linux_x86_64"`,
		`--platform="darwin_arm64"`,
		// Top-level filegroup routes consumers.
		`name = "install_tree.tar"`,
		`["linux_x86_64/install_tree.tar"]`,
		`["darwin_arm64/install_tree.tar"]`,
		`"//conditions:default": [],`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cmake round-2 fallback multi-platform project B missing %q\n%s", want, got)
		}
	}

	// Legacy single-platform genrule name (either historical
	// `demo_install` or the new unsuffixed `demo_trace_build`)
	// must NOT appear under multi-platform mode.
	for _, banned := range []string{
		`name = "demo_install"`,
		`name = "demo_trace_build"`,
	} {
		// `demo_trace_build` is a prefix of the per-platform
		// `demo_trace_build_<plat>` names. Match the exact
		// quoted target name only (the closing `"` after
		// `_trace_build` is sufficient to distinguish).
		if strings.Contains(got, banned+",") || strings.Contains(got, banned+"\n") {
			t.Errorf("cmake round-2 fallback multi-platform project B unexpectedly contains legacy unsuffixed name %q\n%s", banned, got)
		}
	}
}
