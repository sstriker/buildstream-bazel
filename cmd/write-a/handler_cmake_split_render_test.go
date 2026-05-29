package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderCmakeProjectA renders project A for a single trivial kind:cmake
// element and returns the element's project-A BUILD.bazel text.
func renderCmakeProjectA(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(demo C)\nadd_library(thelib STATIC lib.c)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "lib.c"), []byte("int f(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "demo.bst")
	if err := os.WriteFile(bst, []byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, fakeConvertBin(t, tmp)); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestWriter_SplitPackages_RuleShape covers write-a's project-A render
// when --split-packages is on: the element is converted by the
// cmake_split_convert custom rule (a TreeArtifact directory emitter,
// not a genrule), loaded from rules/cmake_packages.bzl, with the
// element's package path and the //tools converter wired through. The
// old build-packages.tar genrule mechanics must be gone.
func TestWriter_SplitPackages_RuleShape(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.splitPackages = true
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	for _, want := range []string{
		`load("@rules_buildstream_bazel//rules:cmake_packages.bzl", "cmake_split_convert")`,
		`cmake_split_convert(`,
		`bazel_package_path = "elements/demo"`,
		`converter = "//tools:convert-element-cmake"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("split-mode project A BUILD missing %q\n%s", want, body)
		}
	}
	// The tar-based genrule mechanism is gone on the split path.
	for _, unwanted := range []string{
		`"build-packages.tar",`,
		`tar -cf`,
		`--out-build="$$PKGTREE/BUILD.bazel"`,
		`"BUILD.bazel.out",`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("split-mode project A BUILD unexpectedly contains %q:\n%s", unwanted, body)
		}
	}
}

// TestWriter_SplitPackages_DepChannel covers the cross-element dep
// channel on the split path: a kind:cmake element with deps emits the
// imports_manifest + exports_in typed attrs (which the cmake_split_convert
// rule turns into --imports-manifest / --exports-in by action-input path),
// and the diagnostics dial emits emit_rejections. A no-dep element emits
// none of them.
func TestWriter_SplitPackages_DepChannel(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.splitPackages = true
	cmakeConfig.diagnostics = true
	t.Cleanup(func() { cmakeConfig = prev })

	elem := &element{Name: "consumer"}
	deps := []cmakeDepBundleLabel{{DepName: "lib", Label: "//elements/lib:cmake_config_bundle"}}
	depExports := []string{"//elements/lib:exports.json"}
	block := cmakeSplitConvertBlock(elem, deps, depExports, "", "", "", "")

	for _, want := range []string{
		`imports_manifest = "imports.json",`,
		`exports_in = ["//elements/lib:exports.json"],`,
		`dep_bundles = ["//elements/lib:cmake_config_bundle"],`,
		`emit_rejections = True,`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("dep-channel split block missing %q\n%s", want, block)
		}
	}

	// No-dep, no-diagnostics element: none of the dep-channel attrs.
	cmakeConfig.diagnostics = false
	bare := cmakeSplitConvertBlock(&element{Name: "solo"}, nil, nil, "", "", "", "")
	for _, unwanted := range []string{"imports_manifest", "exports_in", "dep_bundles", "emit_rejections"} {
		if strings.Contains(bare, unwanted) {
			t.Errorf("no-dep split block unexpectedly contains %q\n%s", unwanted, bare)
		}
	}
}

// TestWriter_SplitPackages_OffShapeUnchanged pins that the default
// (flag off) render keeps the single BUILD.bazel.out genrule and emits
// neither the cmake_split_convert rule nor any --split-packages
// construct — the byte-shape guarantee for the untouched path.
func TestWriter_SplitPackages_OffShapeUnchanged(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.splitPackages = false
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	for _, want := range []string{
		`"BUILD.bazel.out",`,
		`--out-build="$(location BUILD.bazel.out)"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("off-mode project A BUILD missing %q:\n%s", want, body)
		}
	}
	// Assert on split-only *constructs* — the rule name, its load, and
	// the tar output entry — not bare tokens like "--split-packages"
	// that also appear in the template's explanatory comments.
	for _, unwanted := range []string{
		`cmake_split_convert`,
		`rules:cmake_packages.bzl`,
		`"build-packages.tar",`,
		`--out-build="$$PKGTREE/BUILD.bazel"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("off-mode project A BUILD unexpectedly contains split construct %q:\n%s", unwanted, body)
		}
	}
}
