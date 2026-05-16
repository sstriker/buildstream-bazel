// Smoke tests for the cmd/write-a binary. These don't run Bazel —
// they verify the rendered project-A and project-B trees have the
// expected structure and key content. End-to-end Bazel-build
// validation through both projects lives in:
//
//   - make e2e-meta-hello (single-element kind:cmake fixture, Phase 1)
//   - make e2e-meta-stack (multi-element kind:cmake + kind:stack fixture, Phase 2)
//
// both gated on Bazel availability.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMain populates rulesPackagePath before the test bodies run.
// Production main() resolves the path from the --rules-package-path
// flag; tests call writeProjectA / writeProjectB directly without
// going through flag parsing, so the global stays empty unless we
// seed it here. The path resolves relative to this package
// (cmd/write-a/), which is Go's default test working directory, to
// the in-repo rules_buildstream_bazel/ at the repo root.
func TestMain(m *testing.M) {
	abs, err := filepath.Abs("../../rules_buildstream_bazel")
	if err != nil {
		panic("test-setup: resolve rules_buildstream_bazel path: " + err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(abs, "MODULE.bazel")); statErr != nil {
		panic("test-setup: rules_buildstream_bazel/MODULE.bazel missing at " + abs)
	}
	rulesPackagePath = abs
	os.Exit(m.Run())
}

const sampleCmakeBst = `kind: cmake

sources:
- kind: local
  path: src
`

// fakeConvertBin makes a marker file the writer can stat() + copy. The
// writer never executes it inside these tests; rendering doesn't run
// any actions.
func fakeConvertBin(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "convert-element-cmake")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// makeCmakeBst stages a tiny kind:local cmake source tree at
// dir/<name>/src/ and writes <name>.bst pointing at it. Returns the
// .bst path.
func makeCmakeBst(t *testing.T, dir, name string) string {
	t.Helper()
	srcDir := filepath.Join(dir, name+"-src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject("+name+")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(dir, name+".bst")
	body := "kind: cmake\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return bst
}

func TestWriter_HelloWorldShape(t *testing.T) {
	tmp := t.TempDir()

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(t)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "hello.bst")
	if err := os.WriteFile(bstPath, []byte(sampleCmakeBst), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := fakeConvertBin(t, tmp)

	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if len(g.Elements) != 1 || g.Elements[0].Name != "hello" {
		t.Fatalf("Elements = %+v, want [hello]", g.Elements)
	}
	if g.Elements[0].Bst.Kind != "cmake" {
		t.Errorf("Kind = %q, want cmake", g.Elements[0].Bst.Kind)
	}

	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	for _, want := range []string{
		"MODULE.bazel",
		"BUILD.bazel",
		".bazelrc",
		"tools/convert-element-cmake",
		"tools/BUILD.bazel",
		"elements/hello/BUILD.bazel",
		"elements/hello/sources/CMakeLists.txt",
		"tools/sources.json",
	} {
		if _, err := os.Stat(filepath.Join(outA, want)); err != nil {
			t.Errorf("missing rendered file %q in project A: %v", want, err)
		}
	}
	bazelrcA, err := os.ReadFile(filepath.Join(outA, ".bazelrc"))
	if err != nil {
		t.Fatalf("read project A .bazelrc: %v", err)
	}
	for _, want := range []string{
		"--spawn_strategy=sandboxed",
		"--genrule_strategy=sandboxed",
		"--sandbox_default_allow_network=false",
		"--incompatible_strict_action_env",
		// Operator escape valve. Must land AFTER the strict
		// flags so operator overrides take precedence.
		"try-import %workspace%/.bazelrc.operator",
	} {
		if !strings.Contains(string(bazelrcA), want) {
			t.Errorf("project A .bazelrc missing line %q\n--- contents ---\n%s", want, bazelrcA)
		}
	}
	// rules/*.bzl is no longer rendered into project A — the
	// rules live in the in-repo rules_buildstream_bazel package
	// referenced via bazel_dep + local_path_override.
	for _, unwanted := range []string{"rules/zero_files.bzl", "rules/sources.bzl", "rules/traces.bzl", "rules/BUILD.bazel"} {
		if _, err := os.Stat(filepath.Join(outA, unwanted)); !os.IsNotExist(err) {
			t.Errorf("project A unexpectedly rendered %s (rules now load from @rules_buildstream_bazel//rules)", unwanted)
		}
	}
	// MODULE.bazel must declare the bazel_dep + local_path_override.
	moduleBody, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatalf("read MODULE.bazel: %v", err)
	}
	for _, want := range []string{
		`bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")`,
		`local_path_override(`,
		`module_name = "rules_buildstream_bazel"`,
	} {
		if !strings.Contains(string(moduleBody), want) {
			t.Errorf("project A MODULE.bazel missing %q\n%s", want, moduleBody)
		}
	}

	outB := filepath.Join(tmp, "project-B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	for _, want := range []string{
		"MODULE.bazel",
		"BUILD.bazel",
		".bazelrc",
		"elements/hello/BUILD.bazel",
		"elements/hello/CMakeLists.txt",
		// Phase 7b: gazelle metadata files.
		"tools/cc_index.json",
		"tools/python_modules.json",
	} {
		if _, err := os.Stat(filepath.Join(outB, want)); err != nil {
			t.Errorf("missing rendered file %q in project B: %v", want, err)
		}
	}
	bazelrcB, err := os.ReadFile(filepath.Join(outB, ".bazelrc"))
	if err != nil {
		t.Fatalf("read project B .bazelrc: %v", err)
	}
	for _, want := range []string{
		"--spawn_strategy=sandboxed",
		"--genrule_strategy=sandboxed",
		"--sandbox_default_allow_network=false",
		"--incompatible_strict_action_env",
		"try-import %workspace%/.bazelrc.operator",
	} {
		if !strings.Contains(string(bazelrcB), want) {
			t.Errorf("project B .bazelrc missing line %q\n--- contents ---\n%s", want, bazelrcB)
		}
	}
	bModule, err := os.ReadFile(filepath.Join(outB, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	// Phase 7b: MODULE.bazel ships the three gazelle config
	// directives so operator-driven `gazelle fix` runs can wire
	// dep resolution against the converter-emitted metadata
	// files. The directives are inert if gazelle isn't
	// installed.
	for _, want := range []string{
		"# gazelle:cc_indexfile tools/cc_index.json",
		"# gazelle:cc_use_builtin_bzlmod_index true",
		"# gazelle:python_module_mapping tools/python_modules.json",
	} {
		if !strings.Contains(string(bModule), want) {
			t.Errorf("project B MODULE.bazel missing %q:\n%s", want, bModule)
		}
	}
	if !strings.Contains(string(bModule), `bazel_dep(name = "rules_cc"`) {
		t.Errorf("project B MODULE.bazel missing rules_cc bazel_dep:\n%s", bModule)
	}
	// Phase 8: MODULE.bazel includes the operator-owned overlay
	// file, and write-a wrote a stub at the overlay path that
	// the operator can edit. The stub stays comment-only.
	if !strings.Contains(string(bModule), `include("//:overlay.MODULE.bazel")`) {
		t.Errorf("project B MODULE.bazel missing operator-overlay include():\n%s", bModule)
	}
	overlayPath := filepath.Join(outB, "overlay.MODULE.bazel")
	overlay, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatalf("missing overlay.MODULE.bazel stub: %v", err)
	}
	if !strings.Contains(string(overlay), "operator-owned MODULE.bazel fragment") {
		t.Errorf("overlay.MODULE.bazel stub missing operator-facing comment:\n%s", overlay)
	}
	// Phase 8: rewritable-patterns stub (consumed by
	// cmd/relax-keeps). Default empty patterns list so
	// continuous-conversion loops see no behavior change
	// until the operator declares which genrules their
	// gazelle setup can rewrite.
	rewritablePath := filepath.Join(outB, "tools", "gazelle-rewritable.json")
	rewritable, err := os.ReadFile(rewritablePath)
	if err != nil {
		t.Fatalf("missing tools/gazelle-rewritable.json stub: %v", err)
	}
	for _, want := range []string{`"version": 1`, `"patterns": []`, "Operator-owned config consumed by cmd/relax-keeps"} {
		if !strings.Contains(string(rewritable), want) {
			t.Errorf("gazelle-rewritable.json stub missing %q:\n%s", want, rewritable)
		}
	}
	// Phase 8: re-rendering project B must preserve operator
	// edits to BOTH the overlay and the rewritable-patterns
	// config. Simulate edits and re-run write to confirm.
	const operatorOverlayEdit = "# operator-added: pin gazelle\nbazel_dep(name = \"gazelle\", version = \"0.40.0\")\n"
	const operatorRewritableEdit = `{"version": 1, "patterns": [{"name": "protoc", "cmd_contains": "protoc"}]}` + "\n"
	if err := os.WriteFile(overlayPath, []byte(operatorOverlayEdit), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rewritablePath, []byte(operatorRewritableEdit), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("re-render writeProjectB: %v", err)
	}
	afterRerun, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterRerun) != operatorOverlayEdit {
		t.Errorf("write-a clobbered operator's overlay edits on re-render:\nwant: %q\ngot: %q", operatorOverlayEdit, afterRerun)
	}
	afterRewritable, err := os.ReadFile(rewritablePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterRewritable) != operatorRewritableEdit {
		t.Errorf("write-a clobbered operator's rewritable edits on re-render:\nwant: %q\ngot: %q", operatorRewritableEdit, afterRewritable)
	}
	bPlaceholder, err := os.ReadFile(filepath.Join(outB, "elements/hello/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bPlaceholder), "BUILD_NOT_YET_STAGED") {
		t.Errorf("project B element BUILD missing placeholder marker:\n%s", bPlaceholder)
	}

	// The element's BUILD references the staged convert-element-cmake via
	// tools = [//tools:convert-element-cmake], merges sources via the
	// shadow-build cmd, and produces the three expected outputs.
	body, err := os.ReadFile(filepath.Join(outA, "elements/hello/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, marker := range []string{
		`tools = ["//tools:convert-element-cmake"]`,
		`for src in $(SRCS)`,
		`rel="$${src##*sources/}"`,
		`"BUILD.bazel.out"`,
		`"read_paths.json"`,
		`"cmake-config-bundle.tar"`,
		`$(location //tools:convert-element-cmake)`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("rendered BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
}

// TestWriter_CmakeElementStagesDepBundles covers the cross-
// cmake-element staging path: a kind:cmake element whose deps
// list names another kind:cmake element gets the dep's
// cmake-config bundle staged at convert-element-cmake action time
// under $PREFIX/lib/cmake/<dep>/, with --prefix-dir=$PREFIX
// passed to convert-element-cmake. find_package(<DepPkg> CONFIG)
// inside the consumer's CMakeLists then resolves against the
// staged bundle.
//
// Asserts on the rendered consumer's BUILD shape — markers for
// the cross-element label, the per-dep tar extraction loop, and
// the --prefix-dir flag. End-to-end (bazel build through both
// projects) is exercised by a follow-up scripts/meta-cross-cmake.sh
// gate gated on bzlmod-capable bazel.
func TestWriter_CmakeElementStagesDepBundles(t *testing.T) {
	tmp := t.TempDir()
	// Producer cmake element.
	prodSrc := filepath.Join(tmp, "prod-src")
	if err := os.MkdirAll(prodSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prodSrc, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(prod)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prodBst := filepath.Join(tmp, "prod.bst")
	if err := os.WriteFile(prodBst,
		[]byte("kind: cmake\nsources:\n- kind: local\n  path: "+prodSrc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Consumer cmake element with a depends edge on prod.
	consSrc := filepath.Join(tmp, "cons-src")
	if err := os.MkdirAll(consSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consSrc, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(cons)\nfind_package(prod CONFIG)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	consBst := filepath.Join(tmp, "cons.bst")
	body := "kind: cmake\ndepends:\n- prod\nsources:\n- kind: local\n  path: " + consSrc + "\n"
	if err := os.WriteFile(consBst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	binPath := fakeConvertBin(t, tmp)
	g, err := loadGraph([]string{prodBst, consBst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	consBuild, err := os.ReadFile(filepath.Join(outA, "elements/cons/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(consBuild)
	for _, marker := range []string{
		`"//elements/prod:cmake_config_bundle"`,
		`for tar in $(locations //elements/prod:cmake_config_bundle); do`,
		`tar -xf "$$tar" -C "$$PREFIX"`,
		`--prefix-dir="$$PREFIX"`,
		`"imports.json"`,
		`--imports-manifest="$(location imports.json)"`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("consumer BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}

	// The producer side: its BUILD must expose a
	// `cmake_config_bundle` filegroup so consumers can
	// reference it.
	prodBuild, err := os.ReadFile(filepath.Join(outA, "elements/prod/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prodBuild), `name = "cmake_config_bundle"`) {
		t.Errorf("producer BUILD missing cmake_config_bundle filegroup:\n%s", prodBuild)
	}

	// Negative check: a no-deps element shouldn't get the
	// cross-element extract block. Producer has no deps, so
	// its BUILD should NOT carry the prefix-dir flag.
	if strings.Contains(string(prodBuild), `--prefix-dir`) {
		t.Errorf("producer BUILD wrongly carries --prefix-dir (no deps):\n%s", prodBuild)
	}

	// imports.json is rendered next to the consumer's
	// BUILD.bazel and maps the dep's IMPORTED-target name to
	// its in-meta-project Bazel label. Convention-bound:
	// <dep>::<dep> → //elements/<dep>:<dep>.
	importsPath := filepath.Join(outA, "elements/cons/imports.json")
	importsBody, err := os.ReadFile(importsPath)
	if err != nil {
		t.Fatalf("imports.json missing: %v", err)
	}
	for _, marker := range []string{
		`"cmake_target": "prod::prod"`,
		`"bazel_label": "//elements/prod:prod"`,
	} {
		if !strings.Contains(string(importsBody), marker) {
			t.Errorf("consumer imports.json missing marker %q\n--body--\n%s", marker, importsBody)
		}
	}

	// And the producer (no deps) should NOT have an
	// imports.json — the writer skips it for elements with no
	// kind:cmake deps.
	if _, err := os.Stat(filepath.Join(outA, "elements/prod/imports.json")); err == nil {
		t.Errorf("producer wrongly got an imports.json (has no deps)")
	}
}

// TestWriter_AcceptsNonLocalSourceMetadata covers the source-kind
// dispatch story: non-kind:local sources (kind:tar, kind:git_repo,
// etc.) parse cleanly, their URL/Ref/Track metadata is recorded on
// the resolvedSource entry, and staging skips them gracefully.
// Real source-fetch integration with orchestrator/sourcecheckout
// is deferred — render-time succeeds against any source kind, but
// bazel-build of the resulting BUILD would fail without real bytes.
func TestWriter_AcceptsNonLocalSourceMetadata(t *testing.T) {
	tmp := t.TempDir()
	bstPath := filepath.Join(tmp, "x.bst")
	body := `kind: cmake
sources:
- kind: tar
  url: https://example.org/foo.tar.gz
  ref: a1b2c3
`
	if err := os.WriteFile(bstPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v (non-local sources should parse)", err)
	}
	if len(g.Elements[0].Sources) != 1 {
		t.Fatalf("Sources len = %d, want 1", len(g.Elements[0].Sources))
	}
	src := g.Elements[0].Sources[0]
	if src.Kind != "tar" {
		t.Errorf("Sources[0].Kind: got %q, want %q", src.Kind, "tar")
	}
	if src.URL != "https://example.org/foo.tar.gz" {
		t.Errorf("Sources[0].URL: got %q, want %q", src.URL, "https://example.org/foo.tar.gz")
	}
	if src.Ref.Value != "a1b2c3" {
		t.Errorf("Sources[0].Ref.Value: got %q, want %q", src.Ref.Value, "a1b2c3")
	}
	if src.AbsPath != "" {
		t.Errorf("Sources[0].AbsPath should be empty for non-kind:local; got %q", src.AbsPath)
	}
}

func TestWriter_RejectsDuplicateElementName(t *testing.T) {
	tmp := t.TempDir()
	dir1 := filepath.Join(tmp, "d1")
	dir2 := filepath.Join(tmp, "d2")
	bst1 := makeCmakeBst(t, dir1, "shared")
	bst2 := makeCmakeBst(t, dir2, "shared")
	if _, err := loadGraph([]string{bst1, bst2}, ""); err == nil {
		t.Errorf("expected error for duplicate element name, got nil")
	}
}

func TestWriter_GraphTopoSorted(t *testing.T) {
	// Build three cmake elements where leaf <- mid <- root; load them
	// in reverse order and check the graph comes out in dep order.
	tmp := t.TempDir()
	leafBst := makeCmakeBst(t, tmp, "leaf")
	midBst := makeCmakeBst(t, tmp, "mid")
	rootBst := makeCmakeBst(t, tmp, "root")
	// Inject depends: edges by appending to the .bst files.
	if err := appendDepends(midBst, []string{"leaf"}); err != nil {
		t.Fatal(err)
	}
	if err := appendDepends(rootBst, []string{"mid"}); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{rootBst, midBst, leafBst}, "") // reverse order
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	got := []string{}
	for _, e := range g.Elements {
		got = append(got, e.Name)
	}
	want := []string{"leaf", "mid", "root"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("topo order = %v, want %v", got, want)
	}
}

func TestWriter_RejectsCycle(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := makeCmakeBst(t, tmp, "b")
	// a depends on b, b depends on a → cycle.
	if err := appendDepends(a, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if err := appendDepends(b, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGraph([]string{a, b}, ""); err == nil {
		t.Errorf("expected cycle error, got nil")
	}
}

func TestWriter_RejectsMissingDep(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	if err := appendDepends(a, []string{"nonexistent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGraph([]string{a}, ""); err == nil {
		t.Errorf("expected unresolved-dep error, got nil")
	}
}

func TestWriter_RejectsJunctionDep(t *testing.T) {
	// A junction-crossing dep must surface a clear "junctions not
	// yet supported" error, not fall through to the confusing
	// unknown-element path.
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	if err := appendJunctionDep(a, "other.bst", "someproject.bst"); err != nil {
		t.Fatal(err)
	}
	_, err := loadGraph([]string{a}, "")
	if err == nil {
		t.Fatalf("expected junction-dep rejection, got nil")
	}
	if !strings.Contains(err.Error(), "junction") {
		t.Errorf("error %q should mention junctions", err)
	}
}

func TestDiscoverBstGraph_WalksTransitiveDeps(t *testing.T) {
	// Diamond: root depends on a + b; both depend on leaf. Discovery
	// from root must reach all four, with leaf deduped to one entry,
	// and the result must feed loadGraph cleanly.
	tmp := t.TempDir()
	leafBst := makeCmakeBst(t, tmp, "leaf")
	aBst := makeCmakeBst(t, tmp, "a")
	bBst := makeCmakeBst(t, tmp, "b")
	rootBst := makeCmakeBst(t, tmp, "root")
	if err := appendDepends(aBst, []string{"leaf"}); err != nil {
		t.Fatal(err)
	}
	if err := appendDepends(bBst, []string{"leaf"}); err != nil {
		t.Fatal(err)
	}
	if err := appendDepends(rootBst, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}

	got, err := discoverBstGraph(rootBst, "")
	if err != nil {
		t.Fatalf("discoverBstGraph: %v", err)
	}
	want := []string{aBst, bBst, leafBst, rootBst} // sorted
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("discovered = %v, want %v", got, want)
	}
	if _, err := loadGraph(got, ""); err != nil {
		t.Errorf("loadGraph on discovered set: %v", err)
	}
}

func TestDiscoverBstGraph_WithProjectConf(t *testing.T) {
	// With a project.conf in play, dependency references resolve
	// element-root-relative rather than as siblings of the referrer.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte("name: discovery-fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	leafBst := makeCmakeBst(t, tmp, "leaf")
	rootBst := makeCmakeBst(t, tmp, "root")
	if err := appendDepends(rootBst, []string{"leaf.bst"}); err != nil {
		t.Fatal(err)
	}

	got, err := discoverBstGraph(rootBst, "")
	if err != nil {
		t.Fatalf("discoverBstGraph: %v", err)
	}
	want := []string{leafBst, rootBst}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("discovered = %v, want %v", got, want)
	}
}

func TestDiscoverBstGraph_RejectsMissingDepFile(t *testing.T) {
	tmp := t.TempDir()
	rootBst := makeCmakeBst(t, tmp, "root")
	if err := appendDepends(rootBst, []string{"nonexistent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverBstGraph(rootBst, ""); err == nil {
		t.Errorf("expected error for dep with no .bst on disk, got nil")
	}
}

func TestDiscoverBstGraph_RejectsJunctionDep(t *testing.T) {
	// Discovery rejects junction-crossing deps up front, mirroring
	// loadGraph — so it doesn't instead fail trying to resolve the
	// junctioned filename as a missing sibling .bst.
	tmp := t.TempDir()
	rootBst := makeCmakeBst(t, tmp, "root")
	if err := appendJunctionDep(rootBst, "other.bst", "someproject.bst"); err != nil {
		t.Fatal(err)
	}
	_, err := discoverBstGraph(rootBst, "")
	if err == nil {
		t.Fatalf("expected junction-dep rejection, got nil")
	}
	if !strings.Contains(err.Error(), "junction") {
		t.Errorf("error %q should mention junctions", err)
	}
}

func TestWriter_StackElementShape(t *testing.T) {
	tmp := t.TempDir()
	libA := makeCmakeBst(t, tmp, "lib-a")
	libB := makeCmakeBst(t, tmp, "lib-b")
	stack := filepath.Join(tmp, "runtime.bst")
	if err := os.WriteFile(stack,
		[]byte("kind: stack\ndepends:\n- lib-a\n- lib-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph([]string{libA, libB, stack}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	// Topo order: lib-a, lib-b, runtime.
	got := []string{}
	for _, e := range g.Elements {
		got = append(got, e.Name)
	}
	if strings.Join(got, ",") != "lib-a,lib-b,runtime" {
		t.Errorf("topo order = %v, want [lib-a,lib-b,runtime]", got)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	// Project A: cmake elements get the genrule shape; stack gets a
	// no-op marker BUILD (no targets).
	for _, name := range []string{"lib-a", "lib-b", "runtime"} {
		if _, err := os.Stat(filepath.Join(outA, "elements", name, "BUILD.bazel")); err != nil {
			t.Errorf("project A: missing BUILD for %q: %v", name, err)
		}
	}
	stackBuild, err := os.ReadFile(filepath.Join(outA, "elements/runtime/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	// Stack's project-A package declares no actionable targets — only
	// comments. Anchor the check with `(` so the prose doesn't false-
	// positive ("filegroup that …" comment is fine; "filegroup(" call
	// is not).
	for _, banned := range []string{"genrule(", "filegroup(", "cc_library("} {
		if strings.Contains(string(stackBuild), banned) {
			t.Errorf("project A stack BUILD should declare no targets, got %q in:\n%s", banned, stackBuild)
		}
	}

	// Project B: the stack's filegroup references each dep's primary target.
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	stackBBuild, err := os.ReadFile(filepath.Join(outB, "elements/runtime/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`name = "runtime"`,
		`"//elements/lib-a"`,
		`"//elements/lib-b"`,
	} {
		if !strings.Contains(string(stackBBuild), marker) {
			t.Errorf("project B runtime BUILD missing %q\n--body--\n%s", marker, stackBBuild)
		}
	}
}

// TestWriter_AutotoolsElementShape covers kind:autotools: the
// pipelineHandler defaults expand BuildStream's canonical %{autogen}
// / %{configure} / %{make} / %{make-install} chain. Without an
// element-level override the rendered cmd carries the canonical
// autoconf flag set substituted from the project-default (or
// project.conf-overridden) %{prefix} chain.
func TestWriter_AutotoolsElementShape(t *testing.T) {
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
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Bst.Kind != "autotools" {
		t.Fatalf("Kind = %q, want autotools", g.Elements[0].Bst.Kind)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, marker := range []string{
		// Pipeline shape inherited from pipelineHandler.
		`name = "auto_install"`,
		`outs = ["install_tree.tar"]`,
		// All three phase headers render (autotools defaults supply
		// commands for configure / build / install).
		"# === configure ===",
		"# === build ===",
		"# === install ===",
		// Autogen branch detects ./configure and skips regeneration.
		"export NOCONFIGURE=1",
		"if [ -x ./configure ]; then",
		// Canonical autoconf flag set; %{prefix} is the BuildStream
		// stock /usr/local since this test doesn't ship a project.conf.
		"./configure --prefix=/usr/local",
		"--bindir=/usr/local/bin",
		"--libdir=/usr/local/lib",
		// Make + make-install with the runtime sentinel for
		// %{install-root}.
		`make -j1 DESTDIR="$$INSTALL_ROOT" install`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("autotools BUILD missing %q\n--body--\n%s", marker, got)
		}
	}
}

// TestWriter_AutotoolsToolsStagedInProjectB verifies that
// build-tracer + convert-element-trace land in BOTH
// project A's and project B's tools/ directories when the
// trace-driven path is enabled. Foundation for the
// architectural move (docs/three-pass-flow.md) where the
// autotools install genrule lives in project B's BUILD —
// the //tools:build-tracer + //tools:convert-element-trace
// labels need to resolve in B too.
//
// Without this staging, B-side rendering of the install
// genrule (a follow-up PR) would break with "no such
// target" errors at bazel-build time.
func TestWriter_AutotoolsToolsStagedInProjectB(t *testing.T) {
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
	fakeAutotoolsBin := filepath.Join(tmp, "fake-cea")
	if err := os.WriteFile(fakeAutotoolsBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTracerBin := filepath.Join(tmp, "fake-bt")
	if err := os.WriteFile(fakeTracerBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	traceConfig.convertBin = fakeAutotoolsBin
	traceConfig.tracerBin = fakeTracerBin
	t.Cleanup(func() {
		traceConfig.convertBin = ""
		traceConfig.tracerBin = ""
	})

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	outA := filepath.Join(tmp, "A")
	outB := filepath.Join(tmp, "B")
	if err := writeProjectA(g, outA, fakeConvertBin(t, tmp)); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}

	// Both A and B should contain the autotools tools.
	for _, project := range []string{outA, outB} {
		for _, tool := range []string{"build-tracer", "convert-element-trace"} {
			path := filepath.Join(project, "tools", tool)
			info, err := os.Stat(path)
			if err != nil {
				t.Errorf("%s missing: %v", path, err)
				continue
			}
			if info.Mode().Perm()&0o100 == 0 {
				t.Errorf("%s not executable (mode=%v)", path, info.Mode())
			}
		}
		// tools/BUILD.bazel exports both binaries.
		buildBody, err := os.ReadFile(filepath.Join(project, "tools", "BUILD.bazel"))
		if err != nil {
			t.Fatal(err)
		}
		got := string(buildBody)
		for _, marker := range []string{
			`"build-tracer"`,
			`"convert-element-trace"`,
			`exports_files(`,
		} {
			if !strings.Contains(got, marker) {
				t.Errorf("%s/tools/BUILD.bazel missing %q\n--body--\n%s", project, marker, got)
			}
		}
	}
}

// TestWriter_AutotoolsToolsNotStagedWhenDisabled verifies that
// project B's tools/ stays minimal (sources.json only) when
// the trace-driven path is NOT enabled — coarse-pipeline
// elements don't need the autotools binaries, so we shouldn't
// materialize them in B.
func TestWriter_AutotoolsToolsNotStagedWhenDisabled(t *testing.T) {
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
	// traceConfig left zero — trace-driven path disabled.
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatal(err)
	}
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"build-tracer", "convert-element-trace"} {
		path := filepath.Join(outB, "tools", banned)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s should not be staged when trace-driven path is disabled", path)
		}
	}
}

// TestWriter_AutotoolsNativeWraps covers the trace-driven
// autotools native render path. When --convert-element-trace
// + --build-tracer-bin are supplied, the per-element BUILD's
// install genrule wraps the configure/build/install commands in
// build-tracer and appends a convert-element-trace step that
// emits BUILD.bazel.out alongside install_tree.tar — one Bazel
// action with two outputs. Bazel's action cache (buildbarn in
// CI) handles cross-node convergence; no separate registry.
func TestWriter_AutotoolsNativeWraps(t *testing.T) {
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

	// Marker-shaped fake binaries — RenderA only stages them.
	fakeAutotoolsBin := filepath.Join(tmp, "convert-element-trace-fake")
	if err := os.WriteFile(fakeAutotoolsBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTracerBin := filepath.Join(tmp, "build-tracer-fake")
	if err := os.WriteFile(fakeTracerBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	traceConfig.convertBin = fakeAutotoolsBin
	traceConfig.tracerBin = fakeTracerBin
	t.Cleanup(func() {
		traceConfig.convertBin = ""
		traceConfig.tracerBin = ""
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

	// A-side BUILD is now a marker pointing at B (post-architectural
	// move; see docs/three-pass-flow.md and PR #67 follow-up).
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(aBody), "BUILD_IN_PROJECT_B") {
		t.Errorf("A-side BUILD should be a marker pointing at B; got:\n%s", aBody)
	}

	// B-side BUILD now hosts the install genrule + the rest of
	// the trace-driven scaffolding the test already covered.
	body, err := os.ReadFile(filepath.Join(outB, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	// Positive markers: install genrule produces the four
	// outputs (install_tree.tar + the converter's BUILD.bazel.out
	// + make-db.txt + install-mapping.json) in one action. The
	// pipeline cmds are wrapped in build-tracer; the AppendCmd
	// dumps `make -np` and runs convert-element-trace inline.
	for _, marker := range []string{
		`name = "auto_install"`,
		`"install_tree.tar"`,
		`"BUILD.bazel.out"`,
		`"make-db.txt"`,
		`"install-mapping.json"`,
		`"generated-headers.txt"`,
		`"//tools:build-tracer"`,
		`"//tools:convert-element-trace"`,
		`"$$EXEC_ROOT/$(location //tools:build-tracer)"`,
		`--normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT"`,
		`--normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT"`,
		`--out="$$AUTOTOOLS_TRACE"`,
		`$(location //tools:convert-element-trace)`,
		`( make -np 2>/dev/null || true )`,
		`/^#[[:space:]]+Last modified /d`,
		`> "$$EXEC_ROOT/$(location make-db.txt)"`,
		`--make-db="$(location make-db.txt)"`,
		`--generated-headers="$(location generated-headers.txt)"`,
		`PRE_HEADERS_LIST="$$(mktemp)"`,
		`comm -13 "$$PRE_HEADERS_LIST" "$$POST_HEADERS_LIST"`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("native autotools BUILD missing %q\n--body--\n%s", marker, got)
		}
	}
	// Both binaries staged under tools/.
	if _, err := os.Stat(filepath.Join(outA, "tools/convert-element-trace")); err != nil {
		t.Errorf("convert-element-trace not staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outA, "tools/build-tracer")); err != nil {
		t.Errorf("build-tracer not staged: %v", err)
	}
}

// TestWriter_AutotoolsCoarseFallbackWithoutFlags covers the
// fallback path: without --convert-element-trace /
// --build-tracer-bin, the autotools handler renders the
// unmodified coarse install-pipeline shape. No tracer wrap, no
// BUILD.bazel.out output, no extra tools.
func TestWriter_AutotoolsCoarseFallbackWithoutFlags(t *testing.T) {
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
	traceConfig.convertBin = ""
	traceConfig.tracerBin = ""

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `outs = ["install_tree.tar"]`) {
		t.Errorf("coarse fallback should output only install_tree.tar:\n%s", got)
	}
	for _, missing := range []string{
		`BUILD.bazel.out`,
		`AUTOTOOLS_TRACE`,
		`//tools:build-tracer`,
		`//tools:convert-element-trace`,
	} {
		if strings.Contains(got, missing) {
			t.Errorf("coarse fallback wrongly contains %q\n%s", missing, got)
		}
	}
}

// TestWriter_AutotoolsElementHonorsConfLocal covers the per-element
// override path BuildStream documents: `variables: conf-local: ...`
// appends extra flags to ./configure without re-stating the
// surrounding %{conf-args} shape.
func TestWriter_AutotoolsElementHonorsConfLocal(t *testing.T) {
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
	bstBody := `kind: autotools

sources:
- kind: local
  path: ` + srcDir + `

variables:
  conf-local: --enable-static --disable-shared
`
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/auto/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "--enable-static --disable-shared") {
		t.Errorf("conf-local override didn't reach rendered cmd:\n%s", body)
	}
}

// TestWriter_BazelElementPassthrough covers kind:bazel: the
// source tree's BUILD.bazel is staged verbatim into project B,
// project A renders a no-target marker, and write-a doesn't
// generate any rule overlay.
func TestWriter_BazelElementPassthrough(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	authoredBuild := `# Hand-authored BUILD; kind:bazel passes it through verbatim.
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "embedded",
    srcs = ["main.c"],
    visibility = ["//visibility:public"],
)
`
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.bazel"),
		[]byte(authoredBuild), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.c"),
		[]byte("int main(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "embedded.bst")
	bstBody := "kind: bazel\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Bst.Kind != "bazel" {
		t.Fatalf("Kind = %q, want bazel", g.Elements[0].Bst.Kind)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	bzlA, err := os.ReadFile(filepath.Join(outA, "elements/embedded/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"genrule(", "cc_library(", "cc_binary("} {
		if strings.Contains(string(bzlA), banned) {
			t.Errorf("project A bazel BUILD should declare no targets, got %q in:\n%s", banned, bzlA)
		}
	}

	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outB, "elements/embedded/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != authoredBuild {
		t.Errorf("project B BUILD not staged verbatim:\n--got--\n%s\n--want--\n%s", got, authoredBuild)
	}
	// main.c also staged.
	if _, err := os.Stat(filepath.Join(outB, "elements/embedded/main.c")); err != nil {
		t.Errorf("main.c not staged: %v", err)
	}
}

// TestWriter_BazelElementMissingBuildPlaceholder covers the
// misconfiguration path: kind:bazel source without a BUILD
// file gets a placeholder that flags the gap rather than
// silently producing an empty package.
func TestWriter_BazelElementMissingBuildPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "stuff.txt"),
		[]byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "noBuild.bst")
	bstBody := "kind: bazel\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outB := filepath.Join(tmp, "B")
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outB, "elements/noBuild/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "almost certainly a misconfiguration") {
		t.Errorf("missing-BUILD placeholder should flag the misconfiguration:\n%s", body)
	}
}

// TestWriter_BuildFilesDirOverride covers --build-files-dir:
// an operator-supplied <dir>/<name>/BUILD.bazel subtree
// re-stamps the element to kind:bazel, project A becomes a
// no-target marker, and project B's elements/<name>/ carries
// the override's contents (with the element's kind:local
// sources still staged underneath so the override's
// srcs=[...] references resolve).
func TestWriter_BuildFilesDirOverride(t *testing.T) {
	tmp := t.TempDir()
	// kind:cmake source tree — a hand-authored CMakeLists.txt that
	// won't actually be processed because the override flips the
	// element to kind:bazel. The .c file alongside is what the
	// override BUILD references.
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("project(hello)\nadd_executable(hello hello.c)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "hello.c"),
		[]byte("int main(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "hello.bst")
	bstBody := "kind: cmake\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}

	elemOverride := filepath.Join(tmp, "overrides", "hello")
	if err := os.MkdirAll(elemOverride, 0o755); err != nil {
		t.Fatal(err)
	}
	overrideBuild := `load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "hello",
    srcs = ["hello.c"],
    visibility = ["//visibility:public"],
)
`
	if err := os.WriteFile(filepath.Join(elemOverride, "BUILD.bazel"),
		[]byte(overrideBuild), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	// Source resolution happened under kind:cmake — confirm sources
	// were resolved so the override-then-stage path reaches them.
	if len(g.Elements[0].Sources) != 1 || g.Elements[0].Sources[0].AbsPath == "" {
		t.Fatalf("kind:cmake source not resolved before override: %#v", g.Elements[0].Sources)
	}
	if err := applyBuildFileOverrides(g, filepath.Join(tmp, "overrides")); err != nil {
		t.Fatalf("applyBuildFileOverrides: %v", err)
	}
	if g.Elements[0].Bst.Kind != "bazel" {
		t.Fatalf("after override Kind = %q, want bazel", g.Elements[0].Bst.Kind)
	}
	if g.Elements[0].OverrideBuildDir == "" {
		t.Fatalf("after override OverrideBuildDir unset")
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	bzlA, err := os.ReadFile(filepath.Join(outA, "elements/hello/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"genrule(", "cc_library(", "cc_binary("} {
		if strings.Contains(string(bzlA), banned) {
			t.Errorf("project A override BUILD should be a no-target marker, got %q in:\n%s", banned, bzlA)
		}
	}

	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outB, "elements/hello/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `cc_binary(`) || !strings.Contains(string(got), `"hello.c"`) {
		t.Errorf("project B BUILD didn't carry the override's cc_binary:\n%s", got)
	}
	// kind:cmake sources still staged so the override's srcs = [...]
	// references resolve.
	if _, err := os.Stat(filepath.Join(outB, "elements/hello/hello.c")); err != nil {
		t.Errorf("source file not staged alongside override: %v", err)
	}
}

// TestWriter_BuildFilesDirOverrideSubpackages covers the
// multi-BUILD case: an override subtree with a top-level
// BUILD.bazel plus a subpackage BUILD.bazel — both copy
// across, so project B can host //elements/<name>:top AND
// //elements/<name>/sub:nested in one element.
func TestWriter_BuildFilesDirOverrideSubpackages(t *testing.T) {
	tmp := t.TempDir()
	bst := filepath.Join(tmp, "multipkg.bst")
	if err := os.WriteFile(bst, []byte("kind: stack\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	elemOverride := filepath.Join(tmp, "overrides", "multipkg")
	if err := os.MkdirAll(filepath.Join(elemOverride, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	topBuild := `filegroup(name = "top", visibility = ["//visibility:public"])
`
	subBuild := `filegroup(name = "nested", visibility = ["//visibility:public"])
`
	helperBzl := `def helper():
    pass
`
	if err := os.WriteFile(filepath.Join(elemOverride, "BUILD.bazel"),
		[]byte(topBuild), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elemOverride, "sub", "BUILD.bazel"),
		[]byte(subBuild), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elemOverride, "helper.bzl"),
		[]byte(helperBzl), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if err := applyBuildFileOverrides(g, filepath.Join(tmp, "overrides")); err != nil {
		t.Fatalf("applyBuildFileOverrides: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	top, err := os.ReadFile(filepath.Join(outB, "elements/multipkg/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(top), `name = "top"`) {
		t.Errorf("top-level override BUILD not in elements/multipkg/BUILD.bazel:\n%s", top)
	}
	sub, err := os.ReadFile(filepath.Join(outB, "elements/multipkg/sub/BUILD.bazel"))
	if err != nil {
		t.Fatalf("subpackage BUILD not copied: %v", err)
	}
	if !strings.Contains(string(sub), `name = "nested"`) {
		t.Errorf("subpackage override BUILD wrong:\n%s", sub)
	}
	if _, err := os.Stat(filepath.Join(outB, "elements/multipkg/helper.bzl")); err != nil {
		t.Errorf("helper.bzl not copied alongside BUILD: %v", err)
	}
}

// TestWriter_BuildFilesDirOverrideShadowsSourceBuild covers
// the case where the staged source tree already carried a
// BUILD.bazel (a real kind:bazel-style passthrough tree): the
// override wins and the source's BUILD doesn't linger next to
// it. Also covers the BUILD-vs-BUILD.bazel collision: when
// the source ships `BUILD` and the override ships
// `BUILD.bazel`, Bazel rejects packages declaring both, so
// RenderB strips the stale name before the copyTree lands.
func TestWriter_BuildFilesDirOverrideShadowsSourceBuild(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD"),
		[]byte("# stale source BUILD (no .bazel suffix) to verify the strip\n"+
			"filegroup(name = \"stale\")\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.c"),
		[]byte("int main(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "shadowed.bst")
	bstBody := "kind: bazel\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	elemOverride := filepath.Join(tmp, "overrides", "shadowed")
	if err := os.MkdirAll(elemOverride, 0o755); err != nil {
		t.Fatal(err)
	}
	overrideBuild := `load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "shadowed",
    srcs = ["x.c"],
    visibility = ["//visibility:public"],
)
`
	if err := os.WriteFile(filepath.Join(elemOverride, "BUILD.bazel"),
		[]byte(overrideBuild), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if err := applyBuildFileOverrides(g, filepath.Join(tmp, "overrides")); err != nil {
		t.Fatalf("applyBuildFileOverrides: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outB, "elements/shadowed/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `cc_binary(`) {
		t.Errorf("override content not present:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(outB, "elements/shadowed/BUILD")); !os.IsNotExist(err) {
		t.Errorf("stale source BUILD wasn't stripped; would collide with the override's BUILD.bazel: err=%v", err)
	}
}

// TestWriter_BuildFilesDirOverrideSubpackageBuildCollision
// covers the subpackage variant of the BUILD-vs-BUILD.bazel
// collision strip. The element ships a source tree with a
// sub/BUILD; the override ships a sub/BUILD.bazel.
// stripCollidingBuildNames must remove the stale sub/BUILD
// before the override's copyTree lands, otherwise Bazel
// would see both names in the sub/ package and reject it at
// load time. Mirrors the top-level collision case
// (TestWriter_BuildFilesDirOverrideShadowsSourceBuild) but
// at one level of nesting — exercising that the strip
// walks the override tree rather than only checking the
// package root.
func TestWriter_BuildFilesDirOverrideSubpackageBuildCollision(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	subSrc := filepath.Join(srcDir, "sub")
	if err := os.MkdirAll(subSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.bazel"),
		[]byte("# top-level source BUILD (will be shadowed at root by override)\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subSrc, "BUILD"),
		[]byte("# stale sub-package source BUILD (no .bazel suffix)\n"+
			"filegroup(name = \"stale_sub\")\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subSrc, "helper.c"),
		[]byte("int helper(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "withsub.bst")
	bstBody := "kind: bazel\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	elemOverride := filepath.Join(tmp, "overrides", "withsub")
	if err := os.MkdirAll(filepath.Join(elemOverride, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elemOverride, "BUILD.bazel"),
		[]byte("filegroup(name = \"top\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subOverride := `load("@rules_cc//cc:defs.bzl", "cc_library")

cc_library(
    name = "helper",
    srcs = ["helper.c"],
    visibility = ["//visibility:public"],
)
`
	if err := os.WriteFile(filepath.Join(elemOverride, "sub", "BUILD.bazel"),
		[]byte(subOverride), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if err := applyBuildFileOverrides(g, filepath.Join(tmp, "overrides")); err != nil {
		t.Fatalf("applyBuildFileOverrides: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	// Override's sub/BUILD.bazel landed.
	subBzl, err := os.ReadFile(filepath.Join(outB, "elements/withsub/sub/BUILD.bazel"))
	if err != nil {
		t.Fatalf("override sub/BUILD.bazel not copied: %v", err)
	}
	if !strings.Contains(string(subBzl), `cc_library(`) {
		t.Errorf("subpackage override content wrong:\n%s", subBzl)
	}
	// Stale source-shipped sub/BUILD was stripped.
	if _, err := os.Stat(filepath.Join(outB, "elements/withsub/sub/BUILD")); !os.IsNotExist(err) {
		t.Errorf("stale source sub/BUILD wasn't stripped; would collide with override's sub/BUILD.bazel: err=%v", err)
	}
	// Source-shipped helper.c still staged under sub/ (the override
	// references it via srcs = ["helper.c"]).
	if _, err := os.Stat(filepath.Join(outB, "elements/withsub/sub/helper.c")); err != nil {
		t.Errorf("source sub/helper.c not staged: %v", err)
	}
}

// TestWriter_BuildFilesDirOverrideMissingDoesNothing covers
// the no-override path: --build-files-dir set but no matching
// per-element subtree means the element renders under its
// declared kind unchanged.
func TestWriter_BuildFilesDirOverrideMissingDoesNothing(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.bazel"),
		[]byte("# original BUILD from the source tree\nfilegroup(name = \"keep\")\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "untouched.bst")
	bstBody := "kind: bazel\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	overrideDir := filepath.Join(tmp, "overrides")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A subdirectory exists for a *different* name — make sure
	// applyBuildFileOverrides doesn't accidentally treat any
	// stray dir as a match.
	if err := os.MkdirAll(filepath.Join(overrideDir, "someOther"), 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if err := applyBuildFileOverrides(g, overrideDir); err != nil {
		t.Fatalf("applyBuildFileOverrides: %v", err)
	}
	if g.Elements[0].OverrideBuildDir != "" {
		t.Errorf("OverrideBuildDir should be unset when no override exists, got %q",
			g.Elements[0].OverrideBuildDir)
	}
	if g.Elements[0].Bst.Kind != "bazel" {
		t.Errorf("Kind should stay at declared value, got %q", g.Elements[0].Bst.Kind)
	}
}

// TestWriter_ImportElementShape covers kind:import: project-A
// no-target marker; project-B source tree staged verbatim plus a
// filegroup over glob("**/*", exclude=["BUILD.bazel"]).
func TestWriter_ImportElementShape(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "top.txt"), []byte("top\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "imp.bst")
	bstBody := "kind: import\nsources:\n- kind: local\n  path: " + srcDir + "\n"
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Bst.Kind != "import" {
		t.Fatalf("Kind = %q, want import", g.Elements[0].Bst.Kind)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	importA, err := os.ReadFile(filepath.Join(outA, "elements/imp/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"genrule(", "filegroup(", "cc_library("} {
		if strings.Contains(string(importA), banned) {
			t.Errorf("project A import BUILD should declare no targets, got %q in:\n%s", banned, importA)
		}
	}

	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	// Source tree staged verbatim into project B's element package.
	for _, rel := range []string{"top.txt", "sub/nested.txt"} {
		got, err := os.ReadFile(filepath.Join(outB, "elements/imp", rel))
		if err != nil {
			t.Errorf("staged file %q: %v", rel, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(srcDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("staged %q content differs from fixture", rel)
		}
	}
	importB, err := os.ReadFile(filepath.Join(outB, "elements/imp/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`name = "imp"`,
		// Phase 3's buildtools-canonical formatter wraps the
		// glob() call across lines (it has 2 list args).
		// Assert on individual substrings instead of the inline
		// shape.
		`glob(`,
		`"**/*"`,
		`exclude = ["BUILD.bazel"]`,
		`kind:import`,
	} {
		if !strings.Contains(string(importB), marker) {
			t.Errorf("project B import BUILD missing %q\n--body--\n%s", marker, importB)
		}
	}
}

// TestWriter_FilterElementShape covers kind:filter — single-dep
// validation, `config:` parsing of include / exclude / include-
// orphans recorded as comments in the rendered BUILD, and the
// pass-through filegroup-over-one-dep shape.
func TestWriter_FilterElementShape(t *testing.T) {
	tmp := t.TempDir()
	parent := makeCmakeBst(t, tmp, "lib")
	filter := filepath.Join(tmp, "lib-headers.bst")
	body := `kind: filter

depends:
- lib

config:
  include:
  - public-headers
  exclude:
  - runtime
  include-orphans: false
`
	if err := os.WriteFile(filter, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{parent, filter}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	filterA, err := os.ReadFile(filepath.Join(outA, "elements/lib-headers/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"genrule(", "filegroup(", "cc_library("} {
		if strings.Contains(string(filterA), banned) {
			t.Errorf("project A filter BUILD should declare no targets, got %q in:\n%s", banned, filterA)
		}
	}

	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	filterB, err := os.ReadFile(filepath.Join(outB, "elements/lib-headers/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`name = "lib-headers"`,
		`"//elements/lib"`,
		`kind:filter`,
		`# include domains: [public-headers]`,
		`# exclude domains: [runtime]`,
		`# include-orphans: false`,
	} {
		if !strings.Contains(string(filterB), marker) {
			t.Errorf("project B filter BUILD missing %q\n--body--\n%s", marker, filterB)
		}
	}
}

// TestWriter_FilterRejectsMultipleDeps covers the single-dep
// invariant kind:filter enforces — filter is a slice of exactly one
// parent's install tree, so multi-dep filters surface as an error
// from the handler at render time.
func TestWriter_FilterRejectsMultipleDeps(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := makeCmakeBst(t, tmp, "b")
	bad := filepath.Join(tmp, "bad.bst")
	if err := os.WriteFile(bad,
		[]byte("kind: filter\ndepends:\n- a\n- b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{a, b, bad}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	err = writeProjectA(g, outA, binPath)
	if err == nil {
		t.Fatal("expected error for filter with 2 deps, got nil")
	}
	if !strings.Contains(err.Error(), "expected exactly 1 build-dep") {
		t.Errorf("error should name the single-build-dep invariant; got: %v", err)
	}
}

// TestWriter_ComposeElementShape covers kind:compose. Compose is
// rendering-shape-equivalent to kind:stack — the difference is the
// kind: marker and the BUILD comment, both validated below.
func TestWriter_ComposeElementShape(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := makeCmakeBst(t, tmp, "b")
	bundle := filepath.Join(tmp, "bundle.bst")
	if err := os.WriteFile(bundle,
		[]byte("kind: compose\ndepends:\n- a\n- b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{a, b, bundle}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.ByName["bundle"].Bst.Kind != "compose" {
		t.Fatalf("bundle Kind = %q, want compose", g.ByName["bundle"].Bst.Kind)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	composeA, err := os.ReadFile(filepath.Join(outA, "elements/bundle/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	// Compose's project-A package declares no actionable targets.
	for _, banned := range []string{"genrule(", "filegroup(", "cc_library("} {
		if strings.Contains(string(composeA), banned) {
			t.Errorf("project A compose BUILD should declare no targets, got %q in:\n%s", banned, composeA)
		}
	}
	if !strings.Contains(string(composeA), "kind:compose") {
		t.Errorf("project A compose BUILD should carry kind:compose marker:\n%s", composeA)
	}

	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	composeB, err := os.ReadFile(filepath.Join(outB, "elements/bundle/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`name = "bundle"`,
		`"//elements/a"`,
		`"//elements/b"`,
		`kind:compose`,
	} {
		if !strings.Contains(string(composeB), marker) {
			t.Errorf("project B bundle BUILD missing %q\n--body--\n%s", marker, composeB)
		}
	}
}

func TestWriter_ManualElementShape(t *testing.T) {
	tmp := t.TempDir()

	// Trivial source tree the manual element references in its
	// install commands.
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "greeting.txt"),
		[]byte("Hello from kind:manual!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "greet.bst")
	bstBody := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

config:
  install-commands:
  - install -D greeting.txt %{install-root}%{prefix}/share/greeting.txt
`
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if len(g.Elements) != 1 || g.Elements[0].Bst.Kind != "manual" {
		t.Fatalf("Elements = %+v, want one kind:manual", g.Elements)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/greet/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, marker := range []string{
		`name = "greet_install"`,
		`outs = ["install_tree.tar"]`,
		// %{install-root} stays as the runtime sentinel ($$INSTALL_ROOT);
		// %{prefix} expands to /usr/local at codegen time (BuildStream
		// stock default — this fixture has no project.conf to override
		// it the way the real meta-project fixtures do).
		`$$INSTALL_ROOT/usr/local/share/greeting.txt`,
		// Source-staging shadow merge same as cmake handler.
		`for src in $(SRCS)`,
		// install-commands phase header rendered.
		`# === install ===`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("manual element BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// Source file copied into the project-A package.
	if _, err := os.Stat(filepath.Join(outA, "elements/greet/sources/greeting.txt")); err != nil {
		t.Errorf("sources/greeting.txt not staged: %v", err)
	}

	// Project B: placeholder until the driver post-processes the
	// install tarball into a real wrapper.
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	bBuild, err := os.ReadFile(filepath.Join(outB, "elements/greet/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bBuild), "BUILD_NOT_YET_STAGED") {
		t.Errorf("project B kind:manual BUILD missing placeholder marker:\n%s", bBuild)
	}
}

func TestWriter_MakeElementShape(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Makefile"), []byte("all:\n\t@echo build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "build-it.bst")
	bstBody := `kind: make

sources:
- kind: local
  path: ` + srcDir + `
`
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Bst.Kind != "make" {
		t.Fatalf("Kind = %q, want make", g.Elements[0].Bst.Kind)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/build-it/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, marker := range []string{
		// Pipeline shape.
		`name = "build-it_install"`,
		`outs = ["install_tree.tar"]`,
		`for src in $(SRCS)`,
		// kind:make defaults render verbatim — no per-element
		// build/install commands in the .bst, so the handler's
		// pipelineDefaults filled them in.
		"# === build ===",
		"        make",
		"# === install ===",
		`make -j1 DESTDIR="$$INSTALL_ROOT" install`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("kind:make BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// configure-commands and strip-commands have no defaults and no
	// .bst override → no headers for those phases.
	if strings.Contains(got, "# === configure ===") {
		t.Errorf("kind:make BUILD has unexpected configure phase header:\n%s", got)
	}
	if strings.Contains(got, "# === strip ===") {
		t.Errorf("kind:make BUILD has unexpected strip phase header:\n%s", got)
	}
}

func TestWriter_MakeElementOverridesDefaults(t *testing.T) {
	// .bst-supplied build-commands should replace kind:make's
	// default `make`. Verify the override path.
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Makefile"), []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "make-override.bst")
	bstBody := `kind: make

sources:
- kind: local
  path: ` + srcDir + `

config:
  build-commands:
  - make custom-target
`
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/make-override/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "make custom-target") {
		t.Errorf("override build-commands not honored:\n%s", got)
	}
	if strings.Contains(got, "        make\n") {
		t.Errorf("override build-commands didn't replace default `make`:\n%s", got)
	}
	// install-commands has no .bst override → kind:make's default
	// install line still renders.
	if !strings.Contains(got, `make -j1 DESTDIR="$$INSTALL_ROOT" install`) {
		t.Errorf("install default missing despite no .bst override:\n%s", got)
	}
}

// TestWriter_ElementVariablesOverrideProjectDefaults checks the
// per-element variables: layer flows all the way through
// pipelineHandler.RenderA: a .bst that sets prefix=/opt/foo causes
// %{prefix}/share/... in install-commands to render with /opt/foo
// (rather than the default /usr from projectVars).
func TestWriter_ElementVariablesOverrideProjectDefaults(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "vary.bst")
	bstBody := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

variables:
  prefix: /opt/foo

config:
  install-commands:
  - install -D x.txt %{install-root}%{datadir}/x.txt
`
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/vary/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	// %{datadir} = %{prefix}/share, %{prefix} overridden to /opt/foo,
	// so the resolved path is /opt/foo/share. %{install-root} is the
	// runtime sentinel and stays as $$INSTALL_ROOT.
	want := `install -D x.txt $$INSTALL_ROOT/opt/foo/share/x.txt`
	if !strings.Contains(got, want) {
		t.Errorf("variable override not applied; want substring %q in:\n%s", want, got)
	}
	// And the unsubstituted %{prefix} / %{datadir} must not leak.
	for _, leak := range []string{`%{prefix}`, `%{datadir}`} {
		if strings.Contains(got, leak) {
			t.Errorf("unsubstituted reference %q leaked into output:\n%s", leak, got)
		}
	}
}

// TestWriter_UnknownVariableErrors covers the typo path: a .bst
// references %{not-a-real-var} in a phase command, the resolver
// reports the missing variable, and writeProjectA surfaces it.
func TestWriter_UnknownVariableErrors(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "typo.bst")
	bstBody := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

config:
  install-commands:
  - install -D x.txt %{install-root}%{nonexistent-prefix}/x.txt
`
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	err = writeProjectA(g, outA, binPath)
	if err == nil {
		t.Fatal("expected error for unresolved variable, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-prefix") {
		t.Errorf("error should name the missing variable; got: %v", err)
	}
}

// TestWriter_ProjectConfVarsFlowThroughLoadGraph is the end-to-end
// project.conf integration: a .bst with no element variables, but a
// project.conf alongside that overrides prefix. loadGraph attaches
// the project.conf's variables: to every element via
// element.ProjectConfVars, and pipelineHandler.RenderA layers it
// into the resolver — so the rendered cmd reflects the override.
func TestWriter_ProjectConfVarsFlowThroughLoadGraph(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte("variables:\n  prefix: /opt/projwide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "x.bst")
	bstBody := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

config:
  install-commands:
  - install -D x.txt %{install-root}%{datadir}/x.txt
`
	if err := os.WriteFile(bst, []byte(bstBody), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if got, want := g.Elements[0].ProjectConfVars["prefix"], "/opt/projwide"; got != want {
		t.Errorf("ProjectConfVars[prefix]: got %q, want %q", got, want)
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/x/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	want := `install -D x.txt $$INSTALL_ROOT/opt/projwide/share/x.txt`
	if !strings.Contains(got, want) {
		t.Errorf("project.conf prefix override didn't reach rendered cmd; want substring %q in:\n%s", want, got)
	}
}

// TestWriter_MultiSourceImport covers kind:import with a 2-source
// element. write-a stages each source's tree into project B's
// element package; with no Directory set, the trees merge at the
// element-package root.
func TestWriter_MultiSourceImport(t *testing.T) {
	tmp := t.TempDir()
	srcA := filepath.Join(tmp, "src-a")
	srcB := filepath.Join(tmp, "src-b")
	for _, dir := range []string{srcA, srcB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(srcA, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcB, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "imp.bst")
	body := "kind: import\nsources:\n- kind: local\n  path: " + srcA + "\n- kind: local\n  path: " + srcB + "\n"
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if got := len(g.Elements[0].Sources); got != 2 {
		t.Fatalf("Sources len = %d, want 2", got)
	}
	binPath := fakeConvertBin(t, tmp)
	outB := filepath.Join(tmp, "B")
	if err := os.MkdirAll(outB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	for _, rel := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(outB, "elements/imp", rel)); err != nil {
			t.Errorf("multi-source: %s not staged in project B: %v", rel, err)
		}
	}
}

// TestWriter_SourceDirectoryMountsUnderSubpath covers the source-
// level `directory:` flag: a kind:local source with directory:
// extras stages its content under elemPkg/extras/ rather than at
// the package root.
func TestWriter_SourceDirectoryMountsUnderSubpath(t *testing.T) {
	tmp := t.TempDir()
	srcRoot := filepath.Join(tmp, "src-root")
	srcExtras := filepath.Join(tmp, "src-extras")
	for _, dir := range []string{srcRoot, srcExtras} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "main.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcExtras, "extra.txt"), []byte("extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "imp.bst")
	body := "kind: import\nsources:\n- kind: local\n  path: " + srcRoot + "\n- kind: local\n  path: " + srcExtras + "\n  directory: extras\n"
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if got := g.Elements[0].Sources[1].Directory; got != "extras" {
		t.Errorf("Sources[1].Directory: got %q, want %q", got, "extras")
	}
	binPath := fakeConvertBin(t, tmp)
	outB := filepath.Join(tmp, "B")
	if err := os.MkdirAll(outB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outB, "elements/imp/main.txt")); err != nil {
		t.Errorf("primary source not at element root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outB, "elements/imp/extras/extra.txt")); err != nil {
		t.Errorf("source with directory:extras not staged under extras/: %v", err)
	}
}

// TestWriter_MultiSourcePipeline covers kind:manual with two
// kind:local sources — one mounted at the source root, one under a
// `directory:` subpath. The genrule's source-stage block sees both
// in elemPkg/sources/, with the second under sources/extras/.
func TestWriter_MultiSourcePipeline(t *testing.T) {
	tmp := t.TempDir()
	primary := filepath.Join(tmp, "primary")
	patches := filepath.Join(tmp, "patches")
	for _, dir := range []string{primary, patches} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(primary, "main.c"), []byte("// main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(patches, "0001.patch"), []byte("--- patch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: manual

sources:
- kind: local
  path: ` + primary + `
- kind: local
  path: ` + patches + `
  directory: patches

config:
  install-commands:
  - echo done
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outA, "elements/elem/sources/main.c")); err != nil {
		t.Errorf("primary source not staged at sources/main.c: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outA, "elements/elem/sources/patches/0001.patch")); err != nil {
		t.Errorf("directory:patches source not staged at sources/patches/0001.patch: %v", err)
	}
}

// TestWriter_PublicBlockTolerated covers the public: data block
// real FDSDK elements declare. write-a doesn't act on it yet but
// must accept it without parse errors.
func TestWriter_PublicBlockTolerated(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "imp.bst")
	body := `kind: import

sources:
- kind: local
  path: ` + srcDir + `

public:
  bst:
    split-rules:
      runtime:
        - "/usr/lib/lib*.so*"
      devel:
        - "/usr/lib/lib*.so"
        - "/usr/include/**"
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v (public: block should be tolerated)", err)
	}
	if g.Elements[0].Bst.Public.IsZero() {
		t.Errorf("public: block should round-trip onto bstFile.Public; got zero node")
	}
}

// TestWriter_ConditionalLowersToSelect covers (?): per-arch
// variable overrides being lowered to a project-B
// `cmd = select({...})` in the rendered BUILD. The element's
// install-commands references %{arch-marker} which is set per arch
// via the (?): block; the rendered cmd has one branch per
// supported arch with the per-arch resolved path.
func TestWriter_ConditionalLowersToSelect(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "g.txt"), []byte("g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

variables:
  arch-marker: 'unknown'
  (?):
  - target_arch == "x86_64":
      arch-marker: 'x86_64'
  - target_arch == "aarch64":
      arch-marker: 'aarch64'

config:
  install-commands:
  - install -D g.txt %{install-root}%{datadir}/%{arch-marker}.txt
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if len(g.Elements[0].Bst.Conditionals) != 2 {
		t.Errorf("expected 2 (?): branches on bstFile, got %d", len(g.Elements[0].Bst.Conditionals))
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	for _, marker := range []string{
		// cmd attribute is a select() over @platforms//cpu:*.
		"cmd = select({",
		`"@platforms//cpu:x86_64":`,
		`"@platforms//cpu:aarch64":`,
		// Per-arch resolved paths flow through.
		"$$INSTALL_ROOT/usr/local/share/x86_64.txt",
		"$$INSTALL_ROOT/usr/local/share/aarch64.txt",
		// Unsupported / no-matching-branch arches resolve to the
		// unconditional `arch-marker: 'unknown'` default.
		"$$INSTALL_ROOT/usr/local/share/unknown.txt",
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("rendered BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// Element with no arch-affecting variable references should
	// still render single-string cmd. Verified separately by the
	// existing meta-* gates and the dedup-collapse test below.
}

// TestWriter_ConditionalDedupsIdenticalArches covers the dedup-
// collapse path: when all per-arch resolutions produce the same
// rendered cmd (the (?): block existed but didn't actually affect
// any cmd-referenced variable), write-a renders single-string cmd
// rather than a select() with N identical branches.
func TestWriter_ConditionalDedupsIdenticalArches(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "g.txt"), []byte("g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	// (?): sets a `unused-flag` variable per arch, but no command
	// references it. The dedup-collapse should emit single-string cmd.
	body := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

variables:
  (?):
  - target_arch == "x86_64":
      unused-flag: 'x86_64'
  - target_arch == "aarch64":
      unused-flag: 'aarch64'

config:
  install-commands:
  - install -D g.txt %{install-root}%{datadir}/g.txt
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	if strings.Contains(got, "select(") {
		t.Errorf("identical-across-arches resolution should emit single-string cmd, not select(); got:\n%s", got)
	}
}

// TestWriter_KindLocalPathProjectRootRelative covers the FDSDK
// shape: a kind:local source's `path:` resolves against the
// project root (the directory containing project.conf), not
// against the .bst's own directory. boot-keys-prod.bst at
// elements/components/boot-keys-prod.bst declaring
// `path: files/boot-keys/PK.key` resolves to
// <project>/files/boot-keys/PK.key, not
// <project>/elements/components/files/boot-keys/PK.key.
func TestWriter_KindLocalPathProjectRootRelative(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte("element-path: elements\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stage source file at project-root-relative path.
	if err := os.MkdirAll(filepath.Join(tmp, "files/data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "files/data/secret.txt"),
		[]byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Element lives in a deeper subdirectory than the source it
	// references — making the bst-dir-relative-vs-project-root
	// distinction observable.
	if err := os.MkdirAll(filepath.Join(tmp, "elements/components"), 0o755); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elements/components/elem.bst")
	if err := os.WriteFile(bst, []byte(`kind: import
sources:
- kind: local
  path: files/data
`), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	want := filepath.Join(tmp, "files/data")
	if got := g.Elements[0].Sources[0].AbsPath; got != want {
		t.Errorf("kind:local path didn't resolve project-root-relative\n got: %q\nwant: %q", got, want)
	}
	// And the staged content actually appears in project B at the
	// element root.
	binPath := fakeConvertBin(t, tmp)
	outB := filepath.Join(tmp, "B")
	if err := os.MkdirAll(outB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outB, "elements/components/elem/secret.txt")); err != nil {
		t.Errorf("project-root-relative source didn't stage into project B: %v", err)
	}
}

// TestWriter_SourceCacheHitStagesAsKindLocal covers the
// --source-cache flow: a non-kind:local source whose key resolves
// to a pre-existing directory under the cache stages as if it
// were kind:local at that path. write-a doesn't fetch — callers
// pre-populate the cache via the orchestrator's source-checkout
// layer or by hand for tests.
func TestWriter_SourceCacheHitStagesAsKindLocal(t *testing.T) {
	tmp := t.TempDir()
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: import
sources:
- kind: git_repo
  url: alias:repo.git
  ref: deadbeef
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(tmp, "cache")
	// First load (no cache) — AbsPath should stay empty.
	loaded, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("first loadGraph: %v", err)
	}
	if loaded.Elements[0].Sources[0].AbsPath != "" {
		t.Fatalf("AbsPath should be empty without --source-cache; got %q", loaded.Elements[0].Sources[0].AbsPath)
	}
	key := sourceKey(loaded.Elements[0].Sources[0])
	if key == "" {
		t.Fatal("sourceKey returned empty for non-kind:local source")
	}
	// Pre-stage the cache.
	keyDir := filepath.Join(cacheDir, key)
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "fetched.txt"), []byte("fetched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Second load with cache populated — AbsPath should resolve.
	loaded2, err := loadGraph([]string{bst}, cacheDir)
	if err != nil {
		t.Fatalf("second loadGraph: %v", err)
	}
	if got := loaded2.Elements[0].Sources[0].AbsPath; got != keyDir {
		t.Errorf("cache-resolved AbsPath: got %q, want %q", got, keyDir)
	}
	// And the fetched content stages into project B.
	binPath := fakeConvertBin(t, tmp)
	outB := filepath.Join(tmp, "B")
	if err := os.MkdirAll(outB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectA(loaded2, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(loaded2, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outB, "elements/elem/fetched.txt")); err != nil {
		t.Errorf("cache-resolved kind:git_repo content didn't stage: %v", err)
	}
}

// TestWriter_SourceKeyDeterministic covers the source-key stability
// story: identical source specs produce identical keys (callers
// rely on this when writing fetched trees back into the cache);
// distinct refs produce distinct keys; kind:local sources produce
// the empty key (no fetching needed).
func TestWriter_SourceKeyDeterministic(t *testing.T) {
	rs := resolvedSource{
		Kind: "git_repo",
		URL:  "alias:repo.git",
		Ref:  yaml.Node{Kind: yaml.ScalarNode, Value: "deadbeef"},
	}
	a := sourceKey(rs)
	b := sourceKey(rs)
	if a != b || a == "" {
		t.Errorf("sourceKey not deterministic / empty: %q vs %q", a, b)
	}
	rs2 := rs
	rs2.Ref = yaml.Node{Kind: yaml.ScalarNode, Value: "cafebabe"}
	if sourceKey(rs2) == a {
		t.Errorf("sourceKey collision across different refs")
	}
	rsLocal := resolvedSource{Kind: "local", AbsPath: "/some/path"}
	if got := sourceKey(rsLocal); got != "" {
		t.Errorf("kind:local sourceKey should be empty; got %q", got)
	}
}

// TestWriter_NonLocalSourceSkippedInStaging covers
// stageAllSources's skip-non-local behavior: an element with one
// kind:local + one kind:git_repo source stages the kind:local
// content into project B but leaves nothing on disk for the
// kind:git_repo entry. Render-time succeeds; bazel-build would
// require the source-fetch integration that's deferred.
func TestWriter_NonLocalSourceSkippedInStaging(t *testing.T) {
	tmp := t.TempDir()
	srcLocal := filepath.Join(tmp, "src-local")
	if err := os.MkdirAll(srcLocal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcLocal, "data.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "imp.bst")
	body := `kind: import

sources:
- kind: local
  path: ` + srcLocal + `
- kind: git_repo
  url: somealias:repo.git
  ref: deadbeef
  track: master
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if len(g.Elements[0].Sources) != 2 {
		t.Fatalf("Sources len = %d, want 2", len(g.Elements[0].Sources))
	}
	binPath := fakeConvertBin(t, tmp)
	outB := filepath.Join(tmp, "B")
	if err := os.MkdirAll(outB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectA(g, filepath.Join(tmp, "A"), binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	// kind:local content was staged.
	if _, err := os.Stat(filepath.Join(outB, "elements/imp/data.txt")); err != nil {
		t.Errorf("kind:local source should be staged: %v", err)
	}
	// kind:git_repo metadata is on the resolvedSource entry; nothing
	// to assert in the staged tree (no bytes available without real
	// fetch).
	gitSrc := g.Elements[0].Sources[1]
	if gitSrc.Kind != "git_repo" {
		t.Errorf("Sources[1].Kind: got %q, want git_repo", gitSrc.Kind)
	}
	if gitSrc.URL != "somealias:repo.git" {
		t.Errorf("Sources[1].URL: got %q, want somealias:repo.git", gitSrc.URL)
	}
	if gitSrc.Ref.Value != "deadbeef" || gitSrc.Track != "master" {
		t.Errorf("Sources[1] ref/track not recorded: ref=%q track=%q", gitSrc.Ref.Value, gitSrc.Track)
	}
}

// TestWriter_AllNonLocalSourcesRendersBuild covers the all-non-local
// case: an element whose every source is kind:git_repo / kind:patch
// / etc. still renders a BUILD (the genrule's source set will be
// empty, but write-a's render layer succeeds). Useful for the
// reality check, where most FDSDK elements declare kind:git_repo.
func TestWriter_AllNonLocalSourcesRendersBuild(t *testing.T) {
	tmp := t.TempDir()
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: manual

sources:
- kind: git_repo
  url: somealias:repo.git
  ref: aabbccdd
- kind: patch
  path: patches/0001.patch

config:
  install-commands:
  - echo done
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v (all-non-local sources should parse)", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v (all-non-local sources should render)", err)
	}
	if _, err := os.Stat(filepath.Join(outA, "elements/elem/BUILD.bazel")); err != nil {
		t.Errorf("BUILD.bazel should render even when no sources stage: %v", err)
	}
}

// TestWriter_ScriptElementShape covers kind:script: a single
// flat config:commands list maps onto pipelineHandler's install-
// commands slot. configure / build / strip phases stay empty;
// the rendered cmd has only the install phase.
func TestWriter_ScriptElementShape(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "g.txt"), []byte("g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: script

sources:
- kind: local
  path: ` + srcDir + `

config:
  commands:
  - mkdir -p %{install-root}%{datadir}/scripts
  - install -D g.txt %{install-root}%{datadir}/scripts/g.txt
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Bst.Kind != "script" {
		t.Fatalf("Kind = %q, want script", g.Elements[0].Bst.Kind)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body2, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body2)
	for _, marker := range []string{
		// install phase rendered; configure/build/strip not.
		"# === install ===",
		"mkdir -p $$INSTALL_ROOT/usr/local/share/scripts",
		"install -D g.txt $$INSTALL_ROOT/usr/local/share/scripts/g.txt",
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("kind:script BUILD missing %q\n--body--\n%s", marker, got)
		}
	}
	for _, banned := range []string{
		"# === configure ===",
		"# === build ===",
		"# === strip ===",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("kind:script BUILD shouldn't have phase %q\n--body--\n%s", banned, got)
		}
	}
}

// TestWriter_OptionTypedConditionalLowersToConfigSettingSelect
// covers the end-to-end option-typed (?): lowering: project.conf
// declares an option (snap_grade), an element's variables: block
// has (?): branches keyed on it, the rendered BUILD has
// config_settings per used (option, value) and the genrule's cmd
// is a select() over those config_settings.
func TestWriter_OptionTypedConditionalLowersToConfigSettingSelect(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte(`options:
  snap_grade:
    type: enum
    variable: snap_grade
    default: devel
    values:
    - devel
    - stable
`), 0o644); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

variables:
  out-marker: 'unknown'
  (?):
  - snap_grade == "devel":
      out-marker: 'dev'
  - snap_grade == "stable":
      out-marker: 'prod'

config:
  install-commands:
  - install -D x.txt %{install-root}/usr/share/%{out-marker}.txt
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	for _, marker := range []string{
		// Per-tuple config_setting names use sorted-by-varname
		// values joined with '_'. Single-dim → just the value.
		`name = "devel"`,
		`name = "stable"`,
		`"//options:snap_grade": "devel"`,
		`"//options:snap_grade": "stable"`,
		// select() arms reference the config_settings.
		`":devel":`,
		`":stable":`,
		// Per-arm bodies differ in the resolved out-marker.
		`install -D x.txt $$INSTALL_ROOT/usr/share/dev.txt`,
		`install -D x.txt $$INSTALL_ROOT/usr/share/prod.txt`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("rendered BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// @platforms//cpu:* labels should NOT appear — this is option
	// dispatch, not platform dispatch.
	if strings.Contains(got, "@platforms//cpu:") {
		t.Errorf("@platforms//cpu:* labels should not appear in option-typed dispatch:\n%s", got)
	}
}

// TestWriter_CrossProductDispatch covers the multi-dispatch
// case: an element whose (?): branches reference both target_arch
// and an option-typed variable produces config_settings that
// combine constraint_values + flag_values, one per cross-product
// tuple, with select() arms keyed on the local config_setting
// labels. Replaces the prior v1 single-dispatch-variable contract.
func TestWriter_CrossProductDispatch(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte(`options:
  snap_grade:
    type: enum
    variable: snap_grade
    default: devel
    values:
    - devel
    - stable
`), 0o644); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

variables:
  arch-marker: 'unknown'
  grade-marker: 'unknown'
  (?):
  - target_arch == "x86_64":
      arch-marker: 'amd64'
  - target_arch == "aarch64":
      arch-marker: 'arm64'
  - snap_grade == "devel":
      grade-marker: 'dev'
  - snap_grade == "stable":
      grade-marker: 'prod'

config:
  install-commands:
  - install -D x.txt %{install-root}/usr/share/%{arch-marker}-%{grade-marker}.txt
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	for _, marker := range []string{
		// Per-tuple config_settings combining constraint_values + flag_values.
		// Tuple keys sorted by varname → "snap_grade_target_arch" name shape.
		`name = "devel_x86_64"`,
		`name = "stable_x86_64"`,
		`name = "devel_aarch64"`,
		`name = "stable_aarch64"`,
		`constraint_values = [`,
		`"@platforms//cpu:x86_64"`,
		`flag_values = {`,
		`"//options:snap_grade": "devel"`,
		`"//options:snap_grade": "stable"`,
		// select() arms reference the local config_settings.
		`":devel_x86_64":`,
		`":stable_x86_64":`,
		// Per-tuple resolved bodies.
		`install -D x.txt $$INSTALL_ROOT/usr/share/amd64-dev.txt`,
		`install -D x.txt $$INSTALL_ROOT/usr/share/amd64-prod.txt`,
		`install -D x.txt $$INSTALL_ROOT/usr/share/arm64-dev.txt`,
		`install -D x.txt $$INSTALL_ROOT/usr/share/arm64-prod.txt`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("rendered BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
}

// TestWriter_OptionsPackageRenderedFromProjectConf covers the
// end-to-end flow: project.conf options: declarations get parsed,
// threaded onto graph.Options, and writeProjectA emits both
// //options/BUILD.bazel (with one string_flag per non-target_arch
// option) and a bazel_skylib bazel_dep in MODULE.bazel.
func TestWriter_OptionsPackageRenderedFromProjectConf(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte(`options:
  prod_keys:
    type: bool
    variable: prod_keys
    default: 'False'
  snap_grade:
    type: enum
    variable: snap_grade
    default: devel
    values:
    - devel
    - stable
  target_arch:
    type: arch
    variable: target_arch
    values:
    - x86_64
    - aarch64
`), 0o644); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bst,
		[]byte("kind: import\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if got := len(g.Options); got != 3 {
		t.Errorf("graph.Options len: got %d, want 3", got)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	// MODULE.bazel declares bazel_skylib for string_flag.
	module, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(module), `bazel_dep(name = "bazel_skylib"`) {
		t.Errorf("MODULE.bazel missing bazel_skylib dep:\n%s", module)
	}
	// //options/BUILD.bazel exists with the non-target_arch options.
	opts, err := os.ReadFile(filepath.Join(outA, "options/BUILD.bazel"))
	if err != nil {
		t.Fatalf("//options/BUILD.bazel not rendered: %v", err)
	}
	for _, marker := range []string{
		`name = "prod_keys"`,
		`name = "snap_grade"`,
	} {
		if !strings.Contains(string(opts), marker) {
			t.Errorf("//options/BUILD.bazel missing %q:\n%s", marker, opts)
		}
	}
	if strings.Contains(string(opts), `name = "target_arch"`) {
		t.Errorf("target_arch should be excluded from //options:\n%s", opts)
	}
}

// TestWriter_NoOptionsNoOptionsPackage covers the no-options
// fixture: writeProjectA doesn't emit //options/BUILD.bazel and
// MODULE.bazel doesn't declare bazel_skylib (keeps the rendered
// tree minimal for fixtures that don't use options).
func TestWriter_NoOptionsNoOptionsPackage(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bst,
		[]byte("kind: import\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outA, "options")); !os.IsNotExist(err) {
		t.Errorf("//options/ should not exist when no options declared; stat: %v", err)
	}
	module, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(module), "bazel_skylib") {
		t.Errorf("MODULE.bazel shouldn't declare bazel_skylib without options:\n%s", module)
	}
}

// TestWriter_EnvironmentRendersExports covers the env-var
// rendering: project.conf-level + element-level environment
// blocks compose (element wins), variable references substitute,
// runtime sentinels (%{install-root}) map to shell-var form, and
// the resulting `export K=V` lines appear in the rendered cmd
// after the standard prelude.
func TestWriter_EnvironmentRendersExports(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte(`variables:
  prefix: /usr
environment:
  LC_ALL: en_US.UTF-8
  SOURCE_DATE_EPOCH: '1320937200'
  PROJECT_OVERRIDE_ME: project-value
`), 0o644); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	body := `kind: manual

sources:
- kind: local
  path: ` + srcDir + `

environment:
  PROJECT_OVERRIDE_ME: element-value
  ELEMENT_ONLY: hello
  RUNTIME_REF: '%{install-root}/runtime'

config:
  install-commands:
  - echo done
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	for _, marker := range []string{
		// Project-level env survives.
		`export LC_ALL="en_US.UTF-8"`,
		`export SOURCE_DATE_EPOCH="1320937200"`,
		// Element overrides project on conflict.
		`export PROJECT_OVERRIDE_ME="element-value"`,
		// Element-only entry survives.
		`export ELEMENT_ONLY="hello"`,
		// Runtime sentinel %{install-root} maps to $$INSTALL_ROOT.
		`export RUNTIME_REF="$$INSTALL_ROOT/runtime"`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("rendered cmd missing env marker %q\n--body--\n%s", marker, got)
		}
	}
	// Project-only-value should NOT survive after element override.
	if strings.Contains(got, `export PROJECT_OVERRIDE_ME="project-value"`) {
		t.Error("element override should win over project-level env value")
	}
}

// TestWriter_CollectManifestHandler covers kind:collect_manifest:
// no-source element with build-depends, project-A genrule emits
// an empty install_tree.tar, project-B placeholder stays.
func TestWriter_CollectManifestHandler(t *testing.T) {
	tmp := t.TempDir()
	parent := makeCmakeBst(t, tmp, "parent")
	bst := filepath.Join(tmp, "manifest.bst")
	if err := os.WriteFile(bst,
		[]byte("kind: collect_manifest\nbuild-depends:\n- parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{parent, bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.ByName["manifest"].Bst.Kind != "collect_manifest" {
		t.Fatalf("Kind = %q, want collect_manifest", g.ByName["manifest"].Bst.Kind)
	}
	if len(g.ByName["manifest"].Deps) != 1 {
		t.Errorf("build-depends should produce one Dep edge; got Deps=%v", g.ByName["manifest"].Deps)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outA, "elements/manifest/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, marker := range []string{
		`name = "manifest_install"`,
		`outs = ["install_tree.tar"]`,
		`EMPTY="$$(mktemp -d)"`,
		// No source staging for collect_manifest.
		`srcs = []`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("collect_manifest BUILD missing marker %q\n--body--\n%s", marker, got)
		}
	}
}

// TestWriter_FDSDKGlueHandlers covers the v1-placeholder
// handlers for the four FDSDK-specific glue kinds — same shape
// as collect_manifest (no-source element with build-depends,
// project-A genrule emits an empty install_tree.tar, project-B
// placeholder stays). One table entry per kind keeps the
// per-kind assertion in lockstep; new placeholder kinds added
// in this PR shape (or future ones following it) can extend
// the table.
func TestWriter_FDSDKGlueHandlers(t *testing.T) {
	cases := []struct {
		kind        string
		elemName    string
		bstSubpath  string
		commentMust string // substring assertion on the project-A BUILD comment
	}{
		{"collect_initial_scripts", "initscripts", "initscripts.bst", "FDSDK-local initial-scripts"},
		{"collect_integration", "integrate", "integrate.bst", "integration-script collector"},
		{"check_forbidden", "forbidden", "forbidden.bst", "forbidden-path CI assertion"},
		{"flatpak_repo", "fp_repo", "fp_repo.bst", "flatpak-repo packager"},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			tmp := t.TempDir()
			parent := makeCmakeBst(t, tmp, "parent")
			bst := filepath.Join(tmp, c.bstSubpath)
			if err := os.WriteFile(bst,
				[]byte("kind: "+c.kind+"\nbuild-depends:\n- parent\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			g, err := loadGraph([]string{parent, bst}, "")
			if err != nil {
				t.Fatalf("loadGraph: %v", err)
			}
			if g.ByName[c.elemName].Bst.Kind != c.kind {
				t.Fatalf("Kind = %q, want %q", g.ByName[c.elemName].Bst.Kind, c.kind)
			}
			if len(g.ByName[c.elemName].Deps) != 1 {
				t.Errorf("build-depends should produce one Dep edge; got Deps=%v", g.ByName[c.elemName].Deps)
			}
			binPath := fakeConvertBin(t, tmp)
			outA := filepath.Join(tmp, "A")
			if err := writeProjectA(g, outA, binPath); err != nil {
				t.Fatalf("writeProjectA: %v", err)
			}
			body, err := os.ReadFile(filepath.Join(outA, "elements/"+c.elemName+"/BUILD.bazel"))
			if err != nil {
				t.Fatal(err)
			}
			got := string(body)
			for _, marker := range []string{
				`name = "` + c.elemName + `_install"`,
				`outs = ["install_tree.tar"]`,
				`EMPTY="$$(mktemp -d)"`,
				`srcs = []`,
				c.commentMust, // per-kind comment distinguisher
			} {
				if !strings.Contains(got, marker) {
					t.Errorf("kind:%s BUILD missing marker %q\n--body--\n%s", c.kind, marker, got)
				}
			}
		})
	}
}

// TestWriter_PathQualifiedDeps covers the FDSDK-shape: element
// names key into the graph by their path relative to the project's
// element-root, so a depends-list reference like
// "components/foo.bst" resolves regardless of which subdir the
// dependent element lives in.
func TestWriter_PathQualifiedDeps(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte("variables:\n  prefix: /usr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// elements/components/foo.bst depends on elements/subdir/bar.bst
	// using the path-qualified form.
	for _, sub := range []string{"components", "subdir"} {
		if err := os.MkdirAll(filepath.Join(tmp, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcDir := filepath.Join(tmp, "subdir-src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(bar)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "subdir/bar.bst"),
		[]byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "components/foo.bst"),
		[]byte("kind: stack\ndepends:\n- subdir/bar.bst\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{
		filepath.Join(tmp, "components/foo.bst"),
		filepath.Join(tmp, "subdir/bar.bst"),
	}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	// Names key by project-relative path (element-path defaults to "."
	// since project.conf doesn't set it).
	want := map[string]bool{"components/foo": true, "subdir/bar": true}
	for name := range g.ByName {
		if !want[name] {
			t.Errorf("unexpected element name %q in graph", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("missing element name %q in graph", name)
	}
	// foo's dep resolves to bar.
	foo := g.ByName["components/foo"]
	if foo == nil {
		t.Fatal("components/foo not in graph")
	}
	if len(foo.Deps) != 1 || foo.Deps[0].Name != "subdir/bar" {
		t.Errorf("path-qualified dep not resolved; got Deps=%v", foo.Deps)
	}
}

// TestWriter_PathQualifiedDeps_ElementPathSubdir covers the FDSDK
// case more precisely: project.conf sets element-path: elements,
// so .bst files at <project>/elements/foo/bar.bst key as "foo/bar"
// rather than "elements/foo/bar".
func TestWriter_PathQualifiedDeps_ElementPathSubdir(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte("variables:\n  prefix: /usr\nelement-path: elements\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"elements/components", "elements/bootstrap"} {
		if err := os.MkdirAll(filepath.Join(tmp, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(b)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "elements/bootstrap/bar.bst"),
		[]byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "elements/components/foo.bst"),
		[]byte("kind: stack\nbuild-depends:\n- bootstrap/bar.bst\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{
		filepath.Join(tmp, "elements/components/foo.bst"),
		filepath.Join(tmp, "elements/bootstrap/bar.bst"),
	}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	// element-path: elements strips the "elements/" prefix, so names
	// are "components/foo" and "bootstrap/bar" (matching FDSDK's
	// dep-reference convention).
	if g.ByName["components/foo"] == nil {
		t.Fatalf("components/foo not in graph; have: %v", keysOf(g.ByName))
	}
	if g.ByName["bootstrap/bar"] == nil {
		t.Fatalf("bootstrap/bar not in graph; have: %v", keysOf(g.ByName))
	}
	foo := g.ByName["components/foo"]
	if len(foo.Deps) != 1 || foo.Deps[0].Name != "bootstrap/bar" {
		t.Errorf("path-qualified build-depends not resolved; got Deps=%v", foo.Deps)
	}
}

// TestWriter_PathQualifiedDeps_SameBasenameDifferentSubdirs covers
// the FDSDK case that broke basename keying: two elements with the
// same basename in different subdirs — like
// elements/components/bzip2.bst and elements/bootstrap/bzip2.bst —
// should be distinguishable by their path-qualified name.
func TestWriter_PathQualifiedDeps_SameBasenameDifferentSubdirs(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "project.conf"),
		[]byte("element-path: elements\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"elements/components", "elements/bootstrap"} {
		if err := os.MkdirAll(filepath.Join(tmp, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "elements/components/dup.bst"),
		[]byte("kind: stack\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "elements/bootstrap/dup.bst"),
		[]byte("kind: stack\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{
		filepath.Join(tmp, "elements/components/dup.bst"),
		filepath.Join(tmp, "elements/bootstrap/dup.bst"),
	}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.ByName["components/dup"] == nil || g.ByName["bootstrap/dup"] == nil {
		t.Errorf("same-basename elements should both key by path; got %v", keysOf(g.ByName))
	}
}

// TestWriter_NoProjectConf_BasenameKeyingFallback covers the
// pre-project.conf code path: no project.conf found means name keying
// stays at basename-only (the existing testdata/meta-project/two-libs/
// fixture relies on this).
func TestWriter_NoProjectConf_BasenameKeyingFallback(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "lib-a")
	b := makeCmakeBst(t, tmp, "lib-b")
	g, err := loadGraph([]string{a, b}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	for _, want := range []string{"lib-a", "lib-b"} {
		if g.ByName[want] == nil {
			t.Errorf("expected basename keying %q without project.conf; got %v", want, keysOf(g.ByName))
		}
	}
}

// keysOf returns a sorted slice of the map keys (for stable error
// messages in the path-qualified-resolution tests above).
func keysOf(m map[string]*element) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestWriter_BuildDependsResolvedIntoDepsGraph covers the
// build-depends key (separate from `depends`) flowing into
// element.Deps. Without explicit handling, yaml.v3 silently drops
// the build-depends list since bstFile didn't have the field;
// adding bstFile.BuildDepends + the loadGraph merge reaches it.
func TestWriter_BuildDependsResolvedIntoDepsGraph(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := filepath.Join(tmp, "b.bst")
	if err := os.WriteFile(b,
		[]byte("kind: stack\nbuild-depends:\n- a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{a, b}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	bElem := g.ByName["b"]
	if bElem == nil {
		t.Fatal("element b not in graph")
	}
	if len(bElem.Deps) != 1 || bElem.Deps[0].Name != "a" {
		t.Errorf("build-depends not resolved into Deps; got Deps=%v", bElem.Deps)
	}
}

// TestWriter_RuntimeDependsResolvedIntoDepsGraph covers the
// runtime-depends key — same shape as build-depends.
func TestWriter_RuntimeDependsResolvedIntoDepsGraph(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := filepath.Join(tmp, "b.bst")
	if err := os.WriteFile(b,
		[]byte("kind: stack\nruntime-depends:\n- a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{a, b}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	bElem := g.ByName["b"]
	if len(bElem.Deps) != 1 || bElem.Deps[0].Name != "a" {
		t.Errorf("runtime-depends not resolved into Deps; got Deps=%v", bElem.Deps)
	}
}

// TestWriter_MergedDependsDedupesByElement covers the duplicate
// case: an element listed in both `depends:` and `build-depends:`
// still produces a single edge in element.Deps (topo sort and
// downstream rendering don't care about edge multiplicity, but
// keeping Deps unique avoids surprising the BUILD renderers).
func TestWriter_MergedDependsDedupesByElement(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := filepath.Join(tmp, "b.bst")
	body := `kind: stack

depends:
- a

build-depends:
- a
`
	if err := os.WriteFile(b, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{a, b}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	bElem := g.ByName["b"]
	if len(bElem.Deps) != 1 {
		t.Errorf("duplicate dep across depends + build-depends should dedupe; got %d edges", len(bElem.Deps))
	}
}

// TestWriter_DepFilenameListExpandsToEdges covers FDSDK's
// "depend on each of these elements with the same shared config:"
// shape:
//
//	build-depends:
//	- filename:
//	  - bootstrap/bzip2.bst
//	  - bootstrap/zlib-ng.bst
//	  config:
//	    location: "%{sysroot}"
//
// Each filename in the list expands to a separate dep edge in
// element.Deps. The shared config: applies to each (recorded but
// inert in v1).
func TestWriter_DepFilenameListExpandsToEdges(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := makeCmakeBst(t, tmp, "b")
	c := makeCmakeBst(t, tmp, "c")
	bad := filepath.Join(tmp, "list.bst")
	body := `kind: stack

build-depends:
- filename:
  - a.bst
  - b.bst
  - c.bst
  config:
    location: "%{sysroot}"
`
	if err := os.WriteFile(bad, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{a, b, c, bad}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	listElem := g.ByName["list"]
	if listElem == nil {
		t.Fatal("list element not in graph")
	}
	if len(listElem.Deps) != 3 {
		t.Errorf("list-form dep should expand to 3 edges; got Deps=%v", listElem.Deps)
	}
	// All three names should appear in Deps.
	wantNames := map[string]bool{"a": true, "b": true, "c": true}
	for _, d := range listElem.Deps {
		if !wantNames[d.Name] {
			t.Errorf("unexpected dep name %q in list expansion", d.Name)
		}
		delete(wantNames, d.Name)
	}
	for n := range wantNames {
		t.Errorf("missing dep %q from list expansion", n)
	}
}

// TestWriter_DepMapFormParsed covers the map-form dep shape:
// "- filename: foo.bst, config: {...}". The dep resolves by
// Filename; config: is parsed and recorded on the bstDep but
// otherwise inert. Junction-targeted deps are a separate case —
// loadGraph rejects them (see TestWriter_RejectsJunctionDep).
func TestWriter_DepMapFormParsed(t *testing.T) {
	tmp := t.TempDir()
	a := makeCmakeBst(t, tmp, "a")
	b := filepath.Join(tmp, "b.bst")
	body := `kind: stack

build-depends:
- filename: a.bst
  config:
    location: "%{sysroot}"
`
	if err := os.WriteFile(b, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{a, b}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	bElem := g.ByName["b"]
	if len(bElem.Deps) != 1 || bElem.Deps[0].Name != "a" {
		t.Errorf("map-form dep not resolved by Filename; got Deps=%v", bElem.Deps)
	}
	// The Config field is recorded on the bstDep entry but inert —
	// verify it round-tripped through the unmarshal.
	if bElem.Bst.BuildDepends[0].Config.IsZero() {
		t.Errorf("dep config not recorded on bstDep")
	}
}

// TestWriter_DepMapFormRequiresFilename covers the malformed map
// shape: a map-form dep without a filename: key surfaces as a parse
// error (without this, the silent default of empty filename would
// flow into graph resolution as a confusing "depends on \"\"").
func TestWriter_DepMapFormRequiresFilename(t *testing.T) {
	tmp := t.TempDir()
	bst := filepath.Join(tmp, "bad.bst")
	body := `kind: stack

build-depends:
- junction: somejunction.bst
`
	if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGraph([]string{bst}, ""); err == nil {
		t.Fatal("expected error for map-form dep without filename, got nil")
	}
}

// appendDepends adds a depends: list to an existing .bst file.
func appendDepends(bstPath string, deps []string) error {
	body, err := os.ReadFile(bstPath)
	if err != nil {
		return err
	}
	body = append(body, "depends:\n"...)
	for _, d := range deps {
		body = append(body, "- "+d+"\n"...)
	}
	return os.WriteFile(bstPath, body, 0o644)
}

// appendJunctionDep appends a map-form `depends:` entry that crosses
// a junction — the shape write-a's parser must reject rather than
// silently ignore.
func appendJunctionDep(bstPath, filename, junction string) error {
	body, err := os.ReadFile(bstPath)
	if err != nil {
		return err
	}
	body = append(body, ("depends:\n- filename: " + filename +
		"\n  junction: " + junction + "\n")...)
	return os.WriteFile(bstPath, body, 0o644)
}

// TestWriter_NonLocalSourceProducesUseRepoBlock asserts the wiring
// from PR #56: an element with a non-kind:local source flows
// through writeProjectA + writeProjectB so that:
//   - tools/sources.json contains one entry per unique source key.
//   - MODULE.bazel for both projects loads the sources extension
//     and use_repo's the corresponding "src_<key>" repo.
//   - rules/sources.bzl is staged into both workspaces.
func TestWriter_NonLocalSourceProducesUseRepoBlock(t *testing.T) {
	tmp := t.TempDir()
	bstPath := filepath.Join(tmp, "x.bst")
	body := `kind: cmake
sources:
- kind: tar
  url: https://example.org/foo.tar.gz
  ref: a1b2c3
`
	if err := os.WriteFile(bstPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)

	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	outB := filepath.Join(tmp, "project-B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}

	// sources.json content: exactly one entry, key matches
	// sourceKey(rs).
	wantKey := sourceKey(g.Elements[0].Sources[0])
	if wantKey == "" {
		t.Fatal("test setup error: sourceKey returned empty for non-local source")
	}
	for _, dir := range []string{outA, outB} {
		raw, err := os.ReadFile(filepath.Join(dir, "tools/sources.json"))
		if err != nil {
			t.Fatalf("%s: read sources.json: %v", dir, err)
		}
		if !strings.Contains(string(raw), wantKey) {
			t.Errorf("%s/tools/sources.json missing key %q:\n%s", dir, wantKey, raw)
		}
		if !strings.Contains(string(raw), `"kind": "tar"`) {
			t.Errorf("%s/tools/sources.json missing kind=tar:\n%s", dir, raw)
		}
	}

	// rules/sources.bzl is NO LONGER rendered into either workspace
	// — sources extension loads from @rules_buildstream_bazel//rules.
	for _, dir := range []string{outA, outB} {
		if _, err := os.Stat(filepath.Join(dir, "rules/sources.bzl")); !os.IsNotExist(err) {
			t.Errorf("%s: rules/sources.bzl unexpectedly rendered", dir)
		}
	}

	// MODULE.bazel loads the extension + use_repo's the key in
	// both projects.
	for _, dir := range []string{outA, outB} {
		raw, err := os.ReadFile(filepath.Join(dir, "MODULE.bazel"))
		if err != nil {
			t.Fatalf("%s: read MODULE.bazel: %v", dir, err)
		}
		got := string(raw)
		for _, want := range []string{
			`use_extension("//rules:sources.bzl", "sources")`,
			`sources.from_json(path = "//tools:sources.json")`,
			`"src_` + wantKey + `"`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s/MODULE.bazel missing %q:\n%s", dir, want, got)
			}
		}
	}
}

// TestWriter_LocalOnlyGraphSkipsUseRepoBlock asserts that a graph
// with only kind:local sources still emits the (empty) sources.json
// and the rules/sources.bzl file (so adding a non-local source
// later doesn't require a write-a structure change), but skips the
// noisy use_extension/use_repo block in MODULE.bazel.
func TestWriter_LocalOnlyGraphSkipsUseRepoBlock(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"), []byte("cmake_minimum_required(VERSION 3.20)\nproject(t)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "hello.bst")
	if err := os.WriteFile(bstPath, []byte(sampleCmakeBst), 0o644); err != nil {
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
	// rules/sources.bzl lives in @rules_buildstream_bazel//rules,
	// not in the rendered project. tools/sources.json is still
	// emitted (write-a's per-project metadata, consumed by the
	// rules-package's sources extension at load time).
	if _, err := os.Stat(filepath.Join(outA, "rules/sources.bzl")); !os.IsNotExist(err) {
		t.Errorf("rules/sources.bzl unexpectedly rendered (loads from @rules_buildstream_bazel//rules)")
	}
	if _, err := os.Stat(filepath.Join(outA, "tools/sources.json")); err != nil {
		t.Errorf("tools/sources.json should still be emitted: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "use_extension") {
		t.Errorf("MODULE.bazel should skip use_extension when no non-local sources:\n%s", raw)
	}
}

// TestWriter_SourceCachePopulatesDigestsInJSON asserts the
// PR #58 wiring: when --source-cache resolves a non-kind:local
// source to an on-disk tree, write-a packs that tree, computes
// its CAS Directory digest, and stamps it into sources.json.
// The repo rule (rules/sources.bzl) reads that digest to build
// its FUSE-mount symlink.
func TestWriter_SourceCachePopulatesDigestsInJSON(t *testing.T) {
	tmp := t.TempDir()

	// Stage a fake source-cache hit at <cache>/<key>/.
	cacheDir := filepath.Join(tmp, "src-cache")
	bstSrc := resolvedSource{
		Kind: "tar",
		URL:  "https://example.org/foo.tar.gz",
		Ref:  yaml.Node{Kind: yaml.ScalarNode, Value: "abc123"},
	}
	wantKey := sourceKey(bstSrc)
	if wantKey == "" {
		t.Fatal("setup: sourceKey empty for non-local source")
	}
	srcRoot := filepath.Join(cacheDir, wantKey)
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "main.c"), []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Element pointing at the matching source (URL/Ref the same so
	// loadElement's source-cache lookup hits).
	bstPath := filepath.Join(tmp, "x.bst")
	body := `kind: cmake
sources:
- kind: tar
  url: https://example.org/foo.tar.gz
  ref: abc123
`
	if err := os.WriteFile(bstPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, cacheDir)
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Sources[0].AbsPath == "" {
		t.Fatal("source-cache hit did not populate AbsPath")
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(outA, "tools/sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	str := string(raw)
	if !strings.Contains(str, `"digest"`) {
		t.Errorf("sources.json should carry a digest for source-cache-hit entries:\n%s", str)
	}
	if !strings.Contains(str, wantKey) {
		t.Errorf("sources.json missing key %q", wantKey)
	}
}

// TestWriter_NoSourceCacheLeavesDigestEmpty asserts the
// graceful-degradation case: when --source-cache isn't passed
// (no AbsPath on the resolvedSource), sources.json carries the
// entry with an empty Digest. The repo rule's empty-tree
// fallback handles that without breaking load() resolution.
func TestWriter_NoSourceCacheLeavesDigestEmpty(t *testing.T) {
	tmp := t.TempDir()
	bstPath := filepath.Join(tmp, "x.bst")
	body := `kind: cmake
sources:
- kind: tar
  url: https://example.org/foo.tar.gz
  ref: abc123
`
	if err := os.WriteFile(bstPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(outA, "tools/sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"digest"`) {
		t.Errorf("entries without a source-cache hit should omit digest:\n%s", raw)
	}
}

// TestWriter_UseFuseSourcesEmitsExtRepoRef asserts the
// PR #60 wiring: with --use-fuse-sources, a kind:cmake element
// whose source resolves through --source-cache renders a
// project-A BUILD that pulls srcs from @src_<key>//:tree
// instead of staging into elements/<name>/sources/.
func TestWriter_UseFuseSourcesEmitsExtRepoRef(t *testing.T) {
	tmp := t.TempDir()

	// Stage a fake source-cache hit for a kind:tar source.
	cacheDir := filepath.Join(tmp, "src-cache")
	srcMeta := resolvedSource{
		Kind: "tar",
		URL:  "https://example.org/foo.tar.gz",
		Ref:  yaml.Node{Kind: yaml.ScalarNode, Value: "abc123"},
	}
	wantKey := sourceKey(srcMeta)
	srcRoot := filepath.Join(cacheDir, wantKey)
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(t)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bstPath := filepath.Join(tmp, "x.bst")
	body := `kind: cmake
sources:
- kind: tar
  url: https://example.org/foo.tar.gz
  ref: abc123
`
	if err := os.WriteFile(bstPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, cacheDir)
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Sources[0].AbsPath == "" {
		t.Fatal("source-cache hit did not populate AbsPath; test setup error")
	}

	// Toggle the flag for this test, restore on exit.
	prev := useFuseSourcesGlobal
	useFuseSourcesGlobal = true
	t.Cleanup(func() { useFuseSourcesGlobal = prev })

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	build, err := os.ReadFile(filepath.Join(outA, "elements/x/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(build)
	// FUSE-mode + narrowing emits enumerated per-file labels:
	// the source-cache fixture has a single CMakeLists.txt, so
	// it surfaces as @src_<k>//:tree_dir/CMakeLists.txt. No
	// zero_stubs since nothing's narrowed away (everything's
	// real in the no-patterns / default case).
	for _, want := range []string{
		`@src_` + wantKey + `//:tree_dir/CMakeLists.txt`,
		`rel="$${src##*tree_dir/}"`,
		`--use-fuse-sources`,
		`--source-key="` + wantKey + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("BUILD missing %q:\n%s", want, got)
		}
	}

	// Ensure the on-disk staging directory was NOT created — the
	// whole point of fuse-sources mode is that bytes live in CAS
	// and are served via FUSE.
	if _, err := os.Stat(filepath.Join(outA, "elements/x/sources")); err == nil {
		t.Errorf("fuse-sources mode should not stage into elements/x/sources/")
	}
}

// TestWriter_UseFuseSourcesFallbackForKindLocal asserts that
// --use-fuse-sources gracefully degrades for kind:local
// elements (which have no source-key, so can't be served via
// @src_<key>//): they take the normal staging path.
func TestWriter_UseFuseSourcesFallbackForKindLocal(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(t)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "hello.bst")
	if err := os.WriteFile(bstPath, []byte(sampleCmakeBst), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatal(err)
	}
	prev := useFuseSourcesGlobal
	useFuseSourcesGlobal = true
	t.Cleanup(func() { useFuseSourcesGlobal = prev })

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	// Staging dir should exist (kind:local fallback).
	if _, err := os.Stat(filepath.Join(outA, "elements/hello/sources/CMakeLists.txt")); err != nil {
		t.Errorf("kind:local should still stage even with --use-fuse-sources: %v", err)
	}
	build, err := os.ReadFile(filepath.Join(outA, "elements/hello/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(build), "@src_") {
		t.Errorf("kind:local BUILD should not reference @src_<key>//: %s", build)
	}
}

// TestWriter_ReadPathsPatternsApply asserts the full PR #61
// flow: a kind:cmake element with a sibling
// <element>.read-paths.txt drives partitioning of source files
// into the staged real set vs zero stubs. Checks the rendered
// project A's BUILD references the zero_files target with
// exactly the excluded paths.
func TestWriter_ReadPathsPatternsApply(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "hello-src")
	if err := os.MkdirAll(filepath.Join(srcDir, "include", "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(t)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.c"), []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "include", "api.h"), []byte("// api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "include", "internal", "private.h"), []byte("// private\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bstPath := filepath.Join(tmp, "hello.bst")
	if err := os.WriteFile(bstPath, []byte(`kind: cmake
sources:
- kind: local
  path: hello-src
`), 0o644); err != nil {
		t.Fatal(err)
	}
	patternsPath := filepath.Join(tmp, "hello.read-paths.txt")
	if err := os.WriteFile(patternsPath, []byte(`include CMakeLists.txt
include include/**/*.h
exclude include/internal/*
`), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if g.Elements[0].Patterns == nil {
		t.Fatal("Patterns not loaded")
	}

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	// CMakeLists.txt + include/api.h should be staged real;
	// main.c (not in any include rule) and include/internal/private.h
	// (excluded) should not.
	for _, want := range []string{"sources/CMakeLists.txt", "sources/include/api.h"} {
		if _, err := os.Stat(filepath.Join(outA, "elements/hello", want)); err != nil {
			t.Errorf("expected %s staged real: %v", want, err)
		}
	}
	for _, dontWant := range []string{"sources/main.c", "sources/include/internal/private.h"} {
		if _, err := os.Stat(filepath.Join(outA, "elements/hello", dontWant)); err == nil {
			t.Errorf("expected %s NOT staged (zero-stub'd); but it exists", dontWant)
		}
	}

	// BUILD.bazel should reference zero_files with both
	// excluded paths.
	build, err := os.ReadFile(filepath.Join(outA, "elements/hello/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(build)
	for _, want := range []string{
		`load("@rules_buildstream_bazel//rules:zero_files.bzl", "zero_files")`,
		`"sources/main.c"`,
		`"sources/include/internal/private.h"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("BUILD missing %q:\n%s", want, got)
		}
	}
}

// TestWriter_NoReadPathsPatternsStagesEverything asserts the
// default-when-absent behaviour: with no <element>.read-paths.txt,
// every source file flows as real (no zero_files).
func TestWriter_NoReadPathsPatternsStagesEverything(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "hello-src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(t)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.c"), []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(tmp, "hello.bst")
	if err := os.WriteFile(bstPath, []byte(`kind: cmake
sources:
- kind: local
  path: hello-src
`), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph([]string{bstPath}, "")
	if err != nil {
		t.Fatal(err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatal(err)
	}
	build, err := os.ReadFile(filepath.Join(outA, "elements/hello/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(build), "zero_files") {
		t.Errorf("default-no-patterns should not emit zero_files:\n%s", build)
	}
	for _, want := range []string{"sources/CMakeLists.txt", "sources/main.c"} {
		if _, err := os.Stat(filepath.Join(outA, "elements/hello", want)); err != nil {
			t.Errorf("expected %s staged real: %v", want, err)
		}
	}
}

// TestWriter_PipelineUseFuseSourcesEmitsExtRepoRef proves PR #62:
// pipeline-shape kinds (autotools/make/manual/script) honor
// --use-fuse-sources for single non-kind:local sources, emitting
// a BUILD that pulls srcs from @src_<key>//:tree (the FUSE
// path) instead of staging into elements/<name>/sources/.
func TestWriter_PipelineUseFuseSourcesEmitsExtRepoRef(t *testing.T) {
	tmp := t.TempDir()

	// Stage a fake source-cache hit for the autotools element's
	// kind:tar source.
	cacheDir := filepath.Join(tmp, "src-cache")
	srcMeta := resolvedSource{
		Kind: "tar",
		URL:  "https://example.org/foo.tar.gz",
		Ref:  yaml.Node{Kind: yaml.ScalarNode, Value: "abc123"},
	}
	wantKey := sourceKey(srcMeta)
	srcRoot := filepath.Join(cacheDir, wantKey)
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "configure"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	bstPath := filepath.Join(tmp, "x.bst")
	body := `kind: autotools
sources:
- kind: tar
  url: https://example.org/foo.tar.gz
  ref: abc123
`
	if err := os.WriteFile(bstPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, cacheDir)
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}

	prev := useFuseSourcesGlobal
	useFuseSourcesGlobal = true
	t.Cleanup(func() { useFuseSourcesGlobal = prev })

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	build, err := os.ReadFile(filepath.Join(outA, "elements/x/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(build)
	for _, want := range []string{
		`srcs = ["@src_` + wantKey + `//:tree"]`,
		`rel="$${src##*tree_dir/}"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("BUILD missing %q:\n%s", want, got)
		}
	}
	// FUSE mode should not emit the local _sources filegroup.
	if strings.Contains(got, "x_sources") {
		t.Errorf("FUSE mode should skip the _sources filegroup:\n%s", got)
	}
	// And no on-disk staging.
	if _, err := os.Stat(filepath.Join(outA, "elements/x/sources")); err == nil {
		t.Errorf("FUSE mode should not stage sources/ on disk for pipeline kinds")
	}
}

// TestWriter_PipelineFuseFallbackForMultiSource proves
// multi-source pipeline elements take the staging path even
// with --use-fuse-sources (no repo composition yet for
// multiple @src_<key>// repos under one element).
func TestWriter_PipelineFuseFallbackForMultiSource(t *testing.T) {
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, "src-cache")
	for _, k := range []struct{ url, ref string }{
		{"https://example.org/a.tar.gz", "v1"},
		{"https://example.org/b.tar.gz", "v2"},
	} {
		key := sourceKey(resolvedSource{Kind: "tar", URL: k.url, Ref: yaml.Node{Kind: yaml.ScalarNode, Value: k.ref}})
		root := filepath.Join(cacheDir, key)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bstPath := filepath.Join(tmp, "y.bst")
	body := `kind: autotools
sources:
- kind: tar
  url: https://example.org/a.tar.gz
  ref: v1
- kind: tar
  url: https://example.org/b.tar.gz
  ref: v2
`
	if err := os.WriteFile(bstPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bstPath}, cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	prev := useFuseSourcesGlobal
	useFuseSourcesGlobal = true
	t.Cleanup(func() { useFuseSourcesGlobal = prev })

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	build, err := os.ReadFile(filepath.Join(outA, "elements/y/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(build)
	if strings.Contains(got, "@src_") {
		t.Errorf("multi-source pipeline should fall back to staging; BUILD references @src_ unexpectedly:\n%s", got)
	}
	if !strings.Contains(got, "y_sources") {
		t.Errorf("multi-source pipeline should emit local _sources filegroup:\n%s", got)
	}
}

// TestWriter_UseFuseSourcesAppliesNarrowing asserts that
// read-paths narrowing applies in FUSE mode: a kind:cmake
// element with a sibling <element>.read-paths.txt produces a
// genrule whose srcs explicitly enumerates only the "real" set
// as @src_<k>//:tree_dir/<path> labels and adds the
// :<elem>_zero_stubs target for the zero set. cmake walks
// SHADOW inside the action — real bytes for real files (CAS-
// served via the labels), empty bytes for zero stubs. Same
// content-stability property the staging-mode narrowing has.
func TestWriter_UseFuseSourcesAppliesNarrowing(t *testing.T) {
	tmp := t.TempDir()

	// Set up a kind:tar source-cache hit with a small tree:
	// CMakeLists.txt + main.c + include/api.h + include/internal/private.h.
	cacheDir := filepath.Join(tmp, "src-cache")
	srcMeta := resolvedSource{
		Kind: "tar",
		URL:  "https://example.org/foo.tar.gz",
		Ref:  yaml.Node{Kind: yaml.ScalarNode, Value: "abc123"},
	}
	wantKey := sourceKey(srcMeta)
	srcRoot := filepath.Join(cacheDir, wantKey)
	if err := os.MkdirAll(filepath.Join(srcRoot, "include", "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(t)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "main.c"), []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "include", "api.h"), []byte("// api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "include", "internal", "private.h"), []byte("// private\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bstPath := filepath.Join(tmp, "x.bst")
	if err := os.WriteFile(bstPath, []byte(`kind: cmake
sources:
- kind: tar
  url: https://example.org/foo.tar.gz
  ref: abc123
`), 0o644); err != nil {
		t.Fatal(err)
	}
	patternsPath := filepath.Join(tmp, "x.read-paths.txt")
	if err := os.WriteFile(patternsPath, []byte(`include CMakeLists.txt
include include/**/*.h
exclude include/internal/*
`), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph([]string{bstPath}, cacheDir)
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}

	prev := useFuseSourcesGlobal
	useFuseSourcesGlobal = true
	t.Cleanup(func() { useFuseSourcesGlobal = prev })

	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "project-A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	build, err := os.ReadFile(filepath.Join(outA, "elements/x/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(build)

	// Real set (enumerated as @src_<k>//:tree_dir/<path> labels):
	for _, want := range []string{
		`@src_` + wantKey + `//:tree_dir/CMakeLists.txt`,
		`@src_` + wantKey + `//:tree_dir/include/api.h`,
		`":x_zero_stubs"`,
		`load("@rules_buildstream_bazel//rules:zero_files.bzl", "zero_files")`,
		// zero_files entries are tree_dir/-prefixed so the cmd's
		// strip pattern recovers the right relative path.
		`"tree_dir/main.c"`,
		`"tree_dir/include/internal/private.h"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("BUILD missing %q:\n%s", want, got)
		}
	}

	// main.c should NOT appear as a real-file label (it's zero-stub'd).
	if strings.Contains(got, `@src_`+wantKey+`//:tree_dir/main.c`) {
		t.Errorf("main.c (excluded) should not appear as a real label; got:\n%s", got)
	}
}
