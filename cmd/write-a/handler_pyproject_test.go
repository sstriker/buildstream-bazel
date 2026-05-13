package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
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

// TestPyprojectElement_FallbackDispatch builds the real
// convert-element-pyproject binary and verifies the per-element
// auto-detection path: a Phase-A-friendly element renders the
// native genrule; a refused-by-Phase-A element renders the
// pipeline shape. End-to-end: real binary + real probe + real
// fixture pyproject.toml files.
func TestPyprojectElement_FallbackDispatch(t *testing.T) {
	tmp := t.TempDir()

	// Build the converter binary into the test's tmp dir; this
	// is what pyprojectConfig.convertBin will point at.
	pyprojectBin := filepath.Join(tmp, "convert-element-pyproject")
	build := exec.Command("go", "build", "-o", pyprojectBin,
		"github.com/sstriker/buildstream-bazel/converter/cmd/convert-element-pyproject")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build convert-element-pyproject: %v", err)
	}

	prevBin, prevFb := pyprojectConfig.convertBin, pyprojectConfig.fallbackEnabled
	prevCache := pyprojectProbeCache
	pyprojectConfig.convertBin = pyprojectBin
	pyprojectConfig.fallbackEnabled = true
	pyprojectProbeCache = map[string]pyprojectProbeResult{}
	defer func() {
		pyprojectConfig.convertBin = prevBin
		pyprojectConfig.fallbackEnabled = prevFb
		pyprojectProbeCache = prevCache
	}()

	// Phase-A-friendly source tree (setuptools).
	friendlySrc := filepath.Join(tmp, "friendly")
	if err := os.MkdirAll(filepath.Join(friendlySrc, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(friendlySrc, "pyproject.toml"),
		[]byte(`[build-system]
requires = ["setuptools>=64"]
build-backend = "setuptools.build_meta"

[project]
name = "demo"
version = "0.1.0"

[tool.setuptools]
packages = ["demo"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(friendlySrc, "demo", "__init__.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Refused source tree (pdm.backend isn't in v1's allow-list).
	refusedSrc := filepath.Join(tmp, "refused")
	if err := os.MkdirAll(refusedSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refusedSrc, "pyproject.toml"),
		[]byte(`[build-system]
requires = ["pdm-backend"]
build-backend = "pdm.backend"

[project]
name = "refused"
version = "0.1.0"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	bstFriendly := filepath.Join(tmp, "friendly.bst")
	if err := os.WriteFile(bstFriendly, []byte(`kind: pyproject

sources:
- kind: local
  path: friendly
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bstRefused := filepath.Join(tmp, "refused.bst")
	if err := os.WriteFile(bstRefused, []byte(`kind: pyproject

sources:
- kind: local
  path: refused
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binPath := fakeConvertBin(t, tmp)
	g, err := loadGraph([]string{bstFriendly, bstRefused}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}

	// Capture stderr across writeProjectA's render so we can
	// assert (a) the refusal diagnostic surfaces for the
	// probe-refused element and (b) it prints exactly once per
	// element across the back-to-back RenderA/RenderB call pair
	// that the cache contract claims to deduplicate.
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = stderrW
	// Restore os.Stderr + close the writer via t.Cleanup so the
	// process-global doesn't stay redirected to the pipe if the
	// rest of this test fails (t.Fatal / panic) before the
	// inline restoration at the bottom runs.
	stderrRestored := false
	t.Cleanup(func() {
		if !stderrRestored {
			os.Stderr = origStderr
			_ = stderrW.Close()
		}
	})
	captured := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, stderrR)
		// io.Copy returns when stderrW is closed and the pipe
		// drains; close the read end too so we don't leak the
		// file descriptor for the rest of the test process.
		_ = stderrR.Close()
		captured <- buf.Bytes()
	}()

	outA := filepath.Join(tmp, "project-A")
	renderErrA := writeProjectA(g, outA, binPath)
	// Drive writeProjectB too so the cache's RenderA→RenderB
	// dedup contract is actually exercised — writeProjectA alone
	// only hits RenderA, and a diagnostic that printed once per
	// project (RenderA emits the marker, RenderB also emits the
	// marker after a cache reset) would still pass a
	// writeProjectA-only assertion. Hitting both confirms the
	// marker prints exactly once across the back-to-back call
	// pair, which is what the cache claims.
	outB := filepath.Join(tmp, "project-B")
	renderErrB := writeProjectB(g, outB)

	os.Stderr = origStderr
	_ = stderrW.Close()
	stderrRestored = true
	capturedStderr := string(<-captured)

	if renderErrA != nil {
		t.Fatalf("writeProjectA: %v\n--captured stderr--\n%s", renderErrA, capturedStderr)
	}
	if renderErrB != nil {
		t.Fatalf("writeProjectB: %v\n--captured stderr--\n%s", renderErrB, capturedStderr)
	}

	friendlyBuild, err := os.ReadFile(filepath.Join(outA, "elements/friendly/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(friendlyBuild), "//tools:convert-element-pyproject") {
		t.Errorf("friendly element should have rendered the native genrule (probe says it's Phase-A-friendly):\n%s", friendlyBuild)
	}

	refusedBuild, err := os.ReadFile(filepath.Join(outA, "elements/refused/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(refusedBuild), "//tools:convert-element-pyproject") {
		t.Errorf("refused element should have fallen back to pipeline shape (probe refuses pdm.backend):\n%s", refusedBuild)
	}
	if !strings.Contains(string(refusedBuild), "install_tree.tar") {
		t.Errorf("refused element's fallback BUILD missing install_tree.tar (expected pipeline shape):\n%s", refusedBuild)
	}

	// Operator-visible refusal diagnostic for the refused
	// element must surface on stderr, exactly once across the
	// back-to-back RenderA/RenderB call pair (the probe cache's
	// once-per-invocation contract).
	wantMarker := "kind:pyproject refused: probe refuses native render"
	count := strings.Count(capturedStderr, wantMarker)
	if count != 1 {
		t.Errorf("refused-element diagnostic should print exactly once on stderr, got %d occurrences of %q\n--captured stderr--\n%s",
			count, wantMarker, capturedStderr)
	}
	// And the friendly element MUST NOT have triggered a refusal
	// diagnostic — the probe should have passed for it.
	if strings.Contains(capturedStderr, "kind:pyproject friendly: probe refuses") {
		t.Errorf("friendly element unexpectedly logged a probe-refusal:\n%s", capturedStderr)
	}
}
