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
	for _, name := range []string{"build-tracer-fake", "trace-publish-fake"} {
		if err := os.WriteFile(filepath.Join(tmp, name),
			[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Snapshot + restore cmakeConfig / autotoolsConfig so the
	// test isn't order-dependent. The validation in main.go
	// requires --build-tracer-bin + --trace-publish-bin to be
	// set when --cmake-round2-fallback is on; the test wires
	// them on autotoolsConfig (the shared resolution target).
	// Explicitly clear the autotools-round2-specific fields so
	// stageAutotoolsTools doesn't see leftover state from a
	// prior test (notably .round2Enabled, which would stage
	// trace-lookup and break the assertions below).
	prevC := cmakeConfig
	prevA := autotoolsConfig
	autotoolsConfig.convertBin = ""
	autotoolsConfig.lookupBin = ""
	autotoolsConfig.round2Enabled = false
	autotoolsConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	autotoolsConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	cmakeConfig.round2FallbackEnabled = true
	t.Cleanup(func() {
		cmakeConfig = prevC
		autotoolsConfig = prevA
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

	// A-side: converter genrule threads the fallback flag.
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(aBody), "--unsupported-execute-process-fallback=true") {
		t.Errorf("project A BUILD missing --unsupported-execute-process-fallback=true flag\n%s", aBody)
	}

	// B-side: real install genrule (no placeholder).
	bBody, err := os.ReadFile(filepath.Join(outB, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "demo_install"`,
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

	// build-tracer + trace-publish stage into both projects'
	// tools/. trace-lookup is NOT staged (kind:cmake fallback
	// v1 doesn't yet consume @trace_<elem>//:trace at A's
	// load time — queued for the trace-driven convergence
	// follow-on).
	for _, project := range []string{outA, outB} {
		for _, tool := range []string{"build-tracer", "trace-publish"} {
			if _, err := os.Stat(filepath.Join(project, "tools", tool)); err != nil {
				t.Errorf("%s missing tools/%s: %v", project, tool, err)
			}
		}
		if _, err := os.Stat(filepath.Join(project, "tools", "trace-lookup")); err == nil {
			t.Errorf("%s unexpectedly staged tools/trace-lookup (fallback-only mode shouldn't need it)", project)
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
	if strings.Contains(string(bBody), `name = "demo_install"`) {
		t.Errorf("project B should NOT emit install genrule when fallback is off; got:\n%s", bBody)
	}
}
