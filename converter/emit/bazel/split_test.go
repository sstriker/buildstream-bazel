package bazel_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_SubdirLibrary_Split_Golden exercises --split-packages on the
// subdir-library fixture: the top-level CMakeLists declares `toplib`
// (root package) and add_subdirectory(src/util) declares `util`
// (src/util package), both including the project-root `include` header
// dir. EmitSplit must:
//   - emit one BUILD.bazel per package (root, src/util, include);
//   - synthesize an `include_headers` cc_library in the include package
//     globbing the headers with includes = ["."];
//   - rewrite toplib's deps to the cross-package util label + the
//     header lib, and re-relativize util's srcs to "util.c";
//   - keep install-derived targets (filegroup, cmake_config_bundle,
//     *_import) in the root package.
//
// Goldens live one-per-package under
// testdata/golden/subdir-library-split/<dir>/BUILD.bazel.golden
// (root → BUILD.bazel.golden), compared with the same scrubSourceLine +
// -update harness the single-BUILD goldens use.
func TestEmit_SubdirLibrary_Split_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/subdir-library"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}

	dirs := make([]string, 0, len(tree))
	for d := range tree {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	goldenDir := filepath.Join("..", "..", "testdata", "golden", "subdir-library-split")
	for _, d := range dirs {
		got := scrubSourceLine(tree[d], src)
		rel := d
		if rel == "" {
			rel = "."
		}
		goldenPath := filepath.Join(goldenDir, rel, "BUILD.bazel.golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated %s", goldenPath)
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("read golden %s (run with -update?): %v", goldenPath, err)
		}
		if string(got) != string(want) {
			t.Errorf("split BUILD.bazel mismatch for dir %q\n--- got ---\n%s\n--- want ---\n%s", d, got, want)
		}
	}

	// Guard the package set so a future change that drops or adds a
	// package surfaces here rather than as a silent golden gap.
	if *update {
		return
	}
	wantDirs := []string{"", "include", "src/util"}
	if len(dirs) != len(wantDirs) {
		t.Fatalf("emitted packages = %v, want %v", dirs, wantDirs)
	}
	for i := range wantDirs {
		if dirs[i] != wantDirs[i] {
			t.Errorf("package set = %v, want %v", dirs, wantDirs)
			break
		}
	}
}

// TestEmit_Split_InterfaceLib_PlacedBySubPackageEntry pins the theme-4
// "absent subpackages" mechanism: for a NON-output-producing target (a library,
// not a genrule/write_file), --split-packages placement is decided by its
// pkg.SubPackages entry, regardless of Kind. (Output-producing rules are the
// exception — landingDir re-homes a genrule/write_file to its primary output's
// package in the local regime so the out is package-local; not exercised here.)
// A codemodel INTERFACE_LIBRARY (KindCCInterface) gets a SubPackages entry in
// the lower (lower.go: every codemodel target does), so it lands in — and
// MATERIALIZES — its own sub-package BUILD.bazel, exactly like a compiled lib.
// A library with NO SubPackages entry (a trace-synthesized interface lib, a
// synthesized filegroup, …) falls to the root package. This is why abseil's
// header-only interface subpackages do get a BUILD.bazel when their libs are
// codemodel-present; an absent subdir BUILD.bazel means the lib was
// trace-synthesized (no entry), not that the target was dropped.
func TestEmit_Split_InterfaceLib_PlacedBySubPackageEntry(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{Name: "rootlib", Kind: ir.KindCCLibrary, Srcs: []string{"root.c"}},
			// Header-only interface lib declared in subdir "sub".
			{Name: "ifacelib", Kind: ir.KindCCInterface, Defines: []string{"IFACE=1"}},
			// Trace-synthesized interface lib: no SubPackages entry.
			{Name: "synthiface", Kind: ir.KindCCInterface, Defines: []string{"SYNTH=1"}},
		},
		SubPackages: map[string]string{
			"rootlib":  "",
			"ifacelib": "sub",
			// "synthiface" deliberately omitted.
		},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	sub, ok := tree["sub"]
	if !ok {
		t.Fatalf("no BUILD.bazel emitted for the interface lib's sub-package; packages = %v", keysOf(tree))
	}
	if !strings.Contains(string(sub), `name = "ifacelib"`) {
		t.Errorf("sub/BUILD.bazel should carry the interface lib; got:\n%s", sub)
	}
	root := string(tree[""])
	if strings.Contains(root, `name = "ifacelib"`) {
		t.Errorf("root BUILD.bazel must NOT carry the sub-package interface lib; got:\n%s", root)
	}
	// The entry-less (trace-synth) interface lib falls to root.
	if !strings.Contains(root, `name = "synthiface"`) {
		t.Errorf("root BUILD.bazel should carry the entry-less interface lib; got:\n%s", root)
	}
}

// TestEmit_Split_SourceKey_LeavesElementRootRelative asserts the
// SourceKey (orchestrator FUSE-sources) regime under --split-packages:
// srcs/hdrs are emitted as @src_<key>//:tree_dir/<element-root-relative>
// absolute labels that are package-location-independent, so the
// transform must NOT re-relativize them to the sub-package — it only
// trims paths in the local (SourceKey=="") regime. Deps still rewrite to
// cross-package labels in both regimes.
func TestEmit_Split_SourceKey_LeavesElementRootRelative(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{
		BazelPackagePath: "elements/subdir-library",
		SourceKey:        "abc",
	})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	util, ok := tree["src/util"]
	if !ok {
		t.Fatalf("no src/util package emitted")
	}
	body := string(util)
	// util.c stays element-root-relative under the @src label, NOT
	// trimmed to "util.c".
	if !contains(body, "@src_abc//:tree_dir/src/util/util.c") {
		t.Errorf("SourceKey regime should keep element-root-relative @src path; got:\n%s", body)
	}
	if contains(body, "\"util.c\"") {
		t.Errorf("SourceKey regime must not re-relativize to a bare \"util.c\"; got:\n%s", body)
	}
	// Deps still rewrite to the cross-package header-lib label.
	if !contains(body, "//elements/subdir-library/include:include_headers") {
		t.Errorf("SourceKey regime should still rewrite deps to cross-package labels; got:\n%s", body)
	}
}

// TestEmit_SplitOff_ByteIdenticalToSingleGolden asserts the OFF
// byte-identity constraint at the EmitSplit boundary: --split-packages
// false (the single-BUILD path) on subdir-library must byte-match the
// existing single-BUILD golden. This complements the unchanged
// TestEmit_SubdirLibrary_Golden by pinning the contract that the
// feature's presence does not perturb the default emit.
func TestEmit_SplitOff_ByteIdenticalToSingleGolden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got, err := bazel.Emit(pkg) // default (split OFF) path
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got = scrubSourceLine(got, src)
	goldenPath := filepath.Join("..", "..", "testdata", "golden", "subdir-library", "BUILD.bazel.golden")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("split-OFF emit drifted from single golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_Split_GenrulePlacedInOutputPackage pins the split-mode
// placement of synthesized genrules: a genrule's outs must live in
// the genrule's own Bazel package, so a genrule producing
// "doc/snippets/compile_x.cpp" must be emitted in the doc/snippets
// package (with outs re-relativized to "compile_x.cpp"), NOT left in
// the root package where it would collide with the doc/snippets
// package and produce a Bazel "output file conflicts with another
// package" error. This is the placement that makes the eigen
// doc-snippet compile_<snippet> re-wire (generated configure_file
// source fed to a cc_binary) actually resolve in --split-packages
// output: the consumer in doc/snippets references the bare-name src
// "compile_x.cpp", which only resolves if the producing genrule is in
// the same package.
func TestEmit_Split_GenrulePlacedInOutputPackage(t *testing.T) {
	pkg := &ir.Package{
		Name: "snippets",
		Targets: []ir.Target{
			{
				Name:       "compile_x",
				Kind:       ir.KindCCBinary,
				Srcs:       []string{"doc/snippets/compile_x.cpp"},
				Visibility: []string{"//visibility:public"},
			},
			{
				Name:        "gen_doc_snippets_compile_x_cpp",
				Kind:        ir.KindGenrule,
				GenruleCmd:  "echo hi > $@",
				GenruleOuts: []string{"doc/snippets/compile_x.cpp"},
				Visibility:  []string{"//visibility:private"},
			},
		},
		// Only the codemodel-derived consumer carries a SubPackages
		// entry; the synthesized genrule has none (defaults to root
		// via targetDir) — the exact shape the placement fix corrects.
		SubPackages: map[string]string{"compile_x": "doc/snippets"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/eigen"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}

	snip, ok := tree["doc/snippets"]
	if !ok {
		t.Fatalf("no doc/snippets package emitted; got dirs %v", keysOf(tree))
	}
	body := string(snip)
	// The genrule landed in doc/snippets with a package-relative out.
	if !contains(body, "gen_doc_snippets_compile_x_cpp") {
		t.Errorf("genrule not placed in doc/snippets package; got:\n%s", body)
	}
	if !contains(body, `outs = ["compile_x.cpp"]`) {
		t.Errorf("genrule outs not re-relativized to package-local \"compile_x.cpp\"; got:\n%s", body)
	}
	// The consumer references the bare-name src in the same package.
	if !contains(body, `"compile_x.cpp"`) {
		t.Errorf("consumer srcs not package-local; got:\n%s", body)
	}
	// The genrule must NOT remain in the root package (where its
	// cross-package out would be illegal).
	if root, ok := tree[""]; ok && contains(string(root), "gen_doc_snippets_compile_x_cpp") {
		t.Errorf("genrule still in root package; should have moved to doc/snippets:\n%s", root)
	}
}

// TestEmit_Split_NestedIncludeRootHeaderLibForwards pins the
// nested-include-root forwarding fix: when include-roots nest (VTK's
// vtk_module_third_party shape — an ancestor forwarder include-root
// plus a deeper one that physically owns the headers), planSplit's
// longest-match header assignment gives every header to the deepest
// root, leaving the ancestor header lib empty. The ancestor lib must
// then DEP on its descendant include-root's header lib so consumers
// that had `-I<ancestor>` on their include path still resolve the
// (recursively reachable) headers — and the otherwise-empty ancestor
// cc_library stops tripping the empty-cc-library finding.
func TestEmit_Split_NestedIncludeRootHeaderLibForwards(t *testing.T) {
	pkg := &ir.Package{
		Name: "vtklike",
		Targets: []ir.Target{
			{
				// Real lib in the deepest package; owns the header and
				// declares its own dir as an include root.
				Name:       "token",
				Kind:       ir.KindCCLibrary,
				Srcs:       []string{"tp/token/vt/token/Token.cxx"},
				Hdrs:       []string{"tp/token/vt/token/Token.h"},
				Includes:   []string{"tp/token/vt/token"},
				Visibility: []string{"//visibility:public"},
			},
			{
				// Consumer that had `-Itp/token` (the ancestor
				// forwarder include-root) and #includes
				// <vt/token/Token.h>.
				Name:       "consumer",
				Kind:       ir.KindCCLibrary,
				Srcs:       []string{"app/use.cxx"},
				Includes:   []string{"tp/token"},
				Visibility: []string{"//visibility:public"},
			},
		},
		SubPackages: map[string]string{
			"token":    "tp/token/vt/token",
			"consumer": "app",
		},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/vtklike"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}

	// The ancestor header lib lives in package tp/token; the deepest
	// header lib (which owns Token.h) lives in tp/token/vt/token.
	ancestor, ok := tree["tp/token"]
	if !ok {
		t.Fatalf("no tp/token package emitted; got dirs %v", keysOf(tree))
	}
	body := string(ancestor)
	// The ancestor header lib must forward to the descendant header lib.
	wantDep := "//elements/vtklike/tp/token/vt/token:tp_token_vt_token_headers"
	if !contains(body, wantDep) {
		t.Errorf("ancestor header lib missing forwarding dep %q; got:\n%s", wantDep, body)
	}
	// The descendant header lib owns Token.h.
	deepest, ok := tree["tp/token/vt/token"]
	if !ok || !contains(string(deepest), "Token.h") {
		t.Errorf("descendant header lib should own Token.h; got:\n%s", deepest)
	}
}

// TestEmit_Split_PkgFilesGlobNotLabelized guards the split-mode glob bug
// the gazelle round-trip harness caught on brotli: a pkg_files from
// install(DIRECTORY) carries a glob PATTERN in Srcs (e.g.
// "c/include/brotli/**"), not a file path. The cross-package src rewrite
// must NOT labelize it into `glob(["//c/include:brotli/**"])` — that's an
// invalid glob (patterns are package-relative, never absolute) and breaks
// the BUILD load. A glob can't cross package boundaries, so once the dir
// is its own package the pattern isn't expressible; drop the src (and the
// now-empty pkg_files) like the bare-packaged-directory case.
func TestEmit_Split_PkgFilesGlobNotLabelized(t *testing.T) {
	pkg := &ir.Package{
		Name: "globlike",
		Targets: []ir.Target{
			{
				Name:        "install_dir_include",
				Kind:        ir.KindPkgFiles,
				Srcs:        []string{"c/include/brotli"},
				PkgSrcsGlob: true,
				Visibility:  []string{"//visibility:public"},
			},
			{
				// A real lib under c/include so that dir becomes its own
				// package (the condition that triggered the labelize bug).
				Name:       "hdrlib",
				Kind:       ir.KindCCLibrary,
				Hdrs:       []string{"c/include/brotli/decode.h"},
				Includes:   []string{"c/include"},
				Visibility: []string{"//visibility:public"},
			},
		},
		SubPackages: map[string]string{"hdrlib": "c/include"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/globlike"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	for dir, body := range tree {
		if contains(string(body), `glob(["//`) {
			t.Errorf("package %q emitted an absolute-label glob (invalid):\n%s", dir, body)
		}
	}
	// The un-expressible pkg_files is dropped, not emitted with an empty
	// or labelized srcs.
	for dir, body := range tree {
		if contains(string(body), "install_dir_include") {
			t.Errorf("package %q kept the un-splittable glob pkg_files; want it dropped:\n%s", dir, body)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// TestEmit_Split_CrossPackageHeaderRelabeled: a target whose Hdrs include a
// header living in a deeper package must reference it by a cross-package
// label (+ raise an exports_files() in the owning package), not drop it.
// Regression guard for the fmt build-lens fix (a test pulling a sibling .cc
// cross-package also needs that package's sibling .h as a compile input).
func TestEmit_Split_CrossPackageHeaderRelabeled(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			{Name: "rootlib", Kind: ir.KindCCLibrary, Srcs: []string{"main.cc"}, Hdrs: []string{"test/util.h"}},
			{Name: "testlib", Kind: ir.KindCCLibrary, Srcs: []string{"test/lib.cc"}},
		},
		SubPackages: map[string]string{"rootlib": "", "testlib": "test"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	root := string(tree[""])
	if !contains(root, "//elements/x/test:util.h") {
		t.Errorf("root pkg: cross-package hdr not relabeled to //elements/x/test:util.h\n%s", root)
	}
	testPkg := string(tree["test"])
	if !contains(testPkg, "exports_files") || !contains(testPkg, `"util.h"`) {
		t.Errorf("test pkg missing exports_files([\"util.h\"])\n%s", testPkg)
	}
}

// TestEmit_Split_IncludeRootHeaderLib_CrossPackageGeneratedSource is the
// OpenBLAS regression: a target declares the element root as an include root
// (includes=["."]), so EmitSplit synthesizes a `root_headers` cc_library that
// aggregates every header under "." with includes=["."]. OpenBLAS's hdrs walk
// sweeps in the per-routine codegen sources it writes under a SUBPACKAGE's
// CMakeFiles/ (e.g. driver/level2/CMakeFiles/cgbmv_c.c, a write_file out that
// driver_level2 also compiles) AND real sibling .h headers in subpackages.
// Under --split-packages the synthesized header lib must:
//   - DROP the cross-package GENERATED compiled source (.c) — it's a
//     translation unit its package compiles, never a header; listing it would
//     emit an invalid same-package label (the subdir is its own package) and,
//     once relabeled, a visibility error against the private write_file rule;
//   - relabel the cross-package real .h to //sub:foo.h + raise exports_files()
//     in the owning package (it's an on-disk source other code may #include).
//
// Before the fix the .c was emitted as a bare "sub/CMakeFiles/x.c" string,
// which Bazel rejects ("'…/sub' is a subpackage").
func TestEmit_Split_IncludeRootHeaderLib_CrossPackageGeneratedSource(t *testing.T) {
	pkg := &ir.Package{
		Name: "ob",
		Targets: []ir.Target{
			// Root lib declaring the element root as an include root; its Hdrs
			// walk pulled in a subpackage's generated .c and a real .h.
			{
				Name:     "rootlib",
				Kind:     ir.KindCCLibrary,
				Srcs:     []string{"main.c"},
				Hdrs:     []string{"sub/CMakeFiles/gen_routine.c", "sub/real.h"},
				Includes: []string{"."},
			},
			// The subpackage that owns + compiles the generated source, and the
			// write_file rule that produces it (so it's a genOut).
			{Name: "sublib", Kind: ir.KindCCLibrary, Srcs: []string{"sub/CMakeFiles/gen_routine.c"}},
			{Name: "gen_sub_routine", Kind: ir.KindWriteFile, WriteFileOut: "sub/CMakeFiles/gen_routine.c"},
		},
		SubPackages: map[string]string{"rootlib": "", "sublib": "sub", "gen_sub_routine": "sub"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/ob"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	hdrLib := ruleBlockAfterName(string(tree[""]), "root_headers")
	if hdrLib == "" {
		t.Fatalf("root pkg missing root_headers lib:\n%s", string(tree[""]))
	}
	// The generated .c must NOT appear at all — neither as a bare subpackage
	// string nor as a cross-package label.
	if strings.Contains(hdrLib, "gen_routine.c") {
		t.Errorf("root_headers must drop the cross-package generated .c; got:\n%s", hdrLib)
	}
	// The real .h must be a cross-package label.
	if !strings.Contains(hdrLib, "//elements/ob/sub:real.h") {
		t.Errorf("root_headers should relabel real.h to //elements/ob/sub:real.h; got:\n%s", hdrLib)
	}
	sub := string(tree["sub"])
	// The owning package exports the real .h (an on-disk source) ...
	if !contains(sub, "exports_files") || !contains(sub, `"real.h"`) {
		t.Errorf("sub pkg missing exports_files([\"real.h\"]):\n%s", sub)
	}
	// ... but NOT the generated .c (already a target; exports_files would
	// conflict with the write_file rule's output).
	if exp := ruleBlockAfterName(sub, "exports_files"); strings.Contains(exp, "gen_routine.c") {
		t.Errorf("sub pkg must NOT exports_files the generated .c:\n%s", sub)
	}
}

// TestEmit_Split_PrivateIncludeHeaderLibWired covers the PRIVATE
// target_include_directories case under --split-packages. A PRIVATE
// include rides Copts as "-I<dir>" (lower keeps it off t.Includes so it
// doesn't propagate). When <dir> is a synthesized header-lib root, the
// bare "-I<dir>" sets the search path but leaves that package's headers
// undeclared as inputs (fmt's posix-mock-test: PRIVATE -Iinclude into
// the split-out //include package for <fmt/os.h>). EmitSplit must wire
// the header lib so its hdrs become inputs — routing it to
// implementation_deps on a cc_library (non-propagating, faithful to
// cmake PRIVATE) and to deps on a cc_test (no implementation_deps
// bucket) — and drop the now-redundant "-I<dir>" copt while keeping
// unrelated copts.
func TestEmit_Split_PrivateIncludeHeaderLibWired(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			// PUBLIC consumer: makes "include" a header-lib root (its own pkg).
			{Name: "pub", Kind: ir.KindCCLibrary, Srcs: []string{"pub.cc"}, Includes: []string{"include"}, Hdrs: []string{"include/foo.h"}},
			// PRIVATE include on a cc_library -> implementation_deps, copt dropped.
			{Name: "privlib", Kind: ir.KindCCLibrary, Srcs: []string{"privlib.cc"}, Copts: []string{"-Iinclude", "-Wall"}},
			// PRIVATE include on a cc_test -> deps, copt dropped.
			{Name: "privtest", Kind: ir.KindCCTest, Srcs: []string{"privtest.cc"}, Copts: []string{"-Iinclude"}},
		},
		SubPackages: map[string]string{"pub": "", "privlib": "", "privtest": ""},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	const hdrLib = "//elements/x/include:include_headers"
	root := string(tree[""])

	lib := ruleBlockAfterName(root, "privlib")
	if !strings.Contains(lib, "implementation_deps = ["+`"`+hdrLib+`"`+"]") {
		t.Errorf("privlib: header lib not in implementation_deps\n%s", lib)
	}
	if strings.Contains(lib, "-Iinclude") {
		t.Errorf("privlib: redundant -Iinclude copt not dropped\n%s", lib)
	}
	if !strings.Contains(lib, `"-Wall"`) {
		t.Errorf("privlib: unrelated -Wall copt was lost\n%s", lib)
	}

	test := ruleBlockAfterName(root, "privtest")
	if !strings.Contains(test, "deps = ["+`"`+hdrLib+`"`+"]") {
		t.Errorf("privtest: header lib not in deps\n%s", test)
	}
	if strings.Contains(test, "implementation_deps") {
		t.Errorf("privtest: cc_test must not get implementation_deps\n%s", test)
	}
	if strings.Contains(test, "-Iinclude") {
		t.Errorf("privtest: redundant -Iinclude copt not dropped\n%s", test)
	}
}

// ruleBlockAfterName returns the slice of a rendered BUILD from the
// `name = "<n>"` attribute line up to the rule's column-0 closing paren —
// enough to assert per-target attributes (deps/copts/implementation_deps)
// without matching a sibling rule in the same file.
func ruleBlockAfterName(build, n string) string {
	i := strings.Index(build, `name = "`+n+`"`)
	if i < 0 {
		return ""
	}
	rest := build[i:]
	if j := strings.Index(rest, "\n)"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestEmit_Split_TextualHdrsRelabeled: a target's textual_hdrs entry that
// belongs to a deeper package (the synthesized textual-source-include lib's
// `src/os.cc`, src/ being its own package) is relabeled to a cross-package
// file label, with exports_files() raised in the owning package — the same
// treatment hdrs/srcs get. Without it the textual_hdr would cross a package
// boundary and fail to load.
func TestEmit_Split_TextualHdrsRelabeled(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			{Name: "t_textual", Kind: ir.KindCCLibrary, TextualHdrs: []string{"src/os.cc"}},
			{Name: "srclib", Kind: ir.KindCCLibrary, Srcs: []string{"src/format.cc"}},
		},
		SubPackages: map[string]string{"t_textual": "", "srclib": "src"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	root := string(tree[""])
	if !contains(root, "//elements/x/src:os.cc") {
		t.Errorf("root pkg: textual_hdr not relabeled to //elements/x/src:os.cc\n%s", root)
	}
	srcPkg := string(tree["src"])
	if !contains(srcPkg, "exports_files") || !contains(srcPkg, `"os.cc"`) {
		t.Errorf("src pkg missing exports_files([\"os.cc\"])\n%s", srcPkg)
	}
}

// TestEmit_Split_AliasActualRelabeled: an alias whose `actual` target splits
// into a subpackage must have its `actual` relabeled to the cross-package
// label. abseil's absl::* / googletest's GTest::* aliases land in the root
// element package (the helper macro declares them there) but point at targets
// that split into subdirs; a bare ":x" there is read by Bazel as a missing
// same-package input file (the abseil/googletest build-lens FAIL).
func TestEmit_Split_AliasActualRelabeled(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			{Name: "absl_foo", Kind: ir.KindAlias, AliasActual: ":foo"},
			{Name: "foo", Kind: ir.KindCCLibrary, Srcs: []string{"absl/foo/foo.cc"}},
		},
		SubPackages: map[string]string{"absl_foo": "", "foo": "absl/foo"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	alias := ruleBlockAfterName(string(tree[""]), "absl_foo")
	if !contains(alias, `actual = "//elements/x/absl/foo:foo"`) {
		t.Errorf("alias actual not relabeled to cross-package label:\n%s", alias)
	}
	if contains(alias, `actual = ":foo"`) {
		t.Errorf("alias still carries the dangling same-package actual:\n%s", alias)
	}
}

// TestEmit_Split_RootIncludeBecomesIncludePrefix: a target whose headers are
// include-rooted at the element root (RootInclude — cmake's
// target_include_directories(${CMAKE_SOURCE_DIR}), which lower can't emit as
// includes=[""]) and which re-homes into a subpackage under the split loses the
// root-relative prefix on its headers (glm/foo.hpp → package-local foo.hpp).
// The emitter must restore it with include_prefix=<package dir> so
// `#include <glm/foo.hpp>` still resolves (the glm compiled-lib build-lens fix).
func TestEmit_Split_RootIncludeBecomesIncludePrefix(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			{Name: "glm", Kind: ir.KindCCLibrary, Srcs: []string{"glm/detail/glm.cpp"}, Hdrs: []string{"glm/glm.hpp", "glm/gtx/q.hpp"}, RootInclude: true},
		},
		SubPackages: map[string]string{"glm": "glm"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	glmPkg := string(tree["glm"])
	if !contains(glmPkg, `include_prefix = "glm"`) {
		t.Errorf("glm pkg: RootInclude target missing include_prefix = \"glm\":\n%s", glmPkg)
	}
	// Headers re-homed package-local (prefix stripped), restored by the prefix.
	if !contains(glmPkg, `"glm.hpp"`) {
		t.Errorf("glm pkg: expected package-local hdr \"glm.hpp\":\n%s", glmPkg)
	}
}

// TestEmit_Split_RootIncludeRootPackageNoPrefix: a RootInclude target that
// stays in the root package (dir == "") needs no include_prefix — its headers
// keep their root-relative path within the single root package.
func TestEmit_Split_RootIncludeRootPackageNoPrefix(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			{Name: "root_lib", Kind: ir.KindCCLibrary, Hdrs: []string{"api.h"}, RootInclude: true},
		},
		SubPackages: map[string]string{"root_lib": ""},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	if contains(string(tree[""]), "include_prefix") {
		t.Errorf("root-package RootInclude target should NOT get include_prefix:\n%s", string(tree[""]))
	}
}

// TestEmit_Split_RootIncludeSourceKeyNoPrefix: in the SourceKey regime hdrs
// stay element-root-relative (they already carry the `glm/` prefix), so a
// RootInclude target must NOT get include_prefix — that would double-prefix
// consumers to `glm/glm/foo.hpp`. The gate is local-regime only.
func TestEmit_Split_RootIncludeSourceKeyNoPrefix(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			{Name: "glm", Kind: ir.KindCCLibrary, Srcs: []string{"glm/detail/glm.cpp"}, Hdrs: []string{"glm/glm.hpp"}, RootInclude: true},
		},
		SubPackages: map[string]string{"glm": "glm"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x", SourceKey: "abc"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	for dir, b := range tree {
		if contains(string(b), "include_prefix") {
			t.Errorf("SourceKey regime must not emit include_prefix (pkg %q):\n%s", dir, string(b))
		}
	}
}

// TestEmit_Split_MultiPackageRootIncludeSynthesizesHeaderLibs: when a
// RootInclude target's element-root header surface spans MORE THAN ONE package
// (abseil's `target_include_directories(${PROJECT_SOURCE_DIR})` grant — every
// such target may include any absl/… header), include_prefix on the target
// itself can't carry headers that re-home into OTHER packages. Instead the
// surface is re-homed into per-package header libs (each with
// include_prefix=<pkg>, or includes=["."] for the root-owned headers in
// non-package dirs like absl/meta) behind one aggregate that every RootInclude
// target depends on.
func TestEmit_Split_MultiPackageRootIncludeSynthesizesHeaderLibs(t *testing.T) {
	// Two libs in different subpackages, each carrying the full cross-package
	// header surface (the shape the discoverHeaders root-walk produces). The
	// surface includes a header in absl/meta, which has no target of its own
	// (so it is NOT a Bazel package and buckets to the root owner).
	surface := []string{"absl/base/casts.h", "absl/strings/str.h", "absl/meta/traits.h"}
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			// base also carries a textual_hdr — NOT part of the root-walk surface,
			// so it must survive the fast-path (re-relativized to package-local),
			// not be dropped alongside the re-homed hdrs.
			{Name: "base", Kind: ir.KindCCLibrary, Srcs: []string{"absl/base/base.cc"}, Hdrs: surface, TextualHdrs: []string{"absl/base/casts.inc"}, RootInclude: true},
			{Name: "strings", Kind: ir.KindCCLibrary, Srcs: []string{"absl/strings/str.cc"}, Hdrs: surface, RootInclude: true},
		},
		SubPackages: map[string]string{"base": "absl/base", "strings": "absl/strings"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/x"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	// The aggregate lives in the root package.
	if ruleBlockAfterName(string(tree[""]), "element_root_headers") == "" {
		t.Errorf("root pkg missing the element_root_headers aggregate:\n%s", string(tree[""]))
	}
	// The per-package header LIB (a distinct rule from the target) carries the
	// prefix that re-exposes the package's headers at their absl/… path.
	baseHdrLib := ruleBlockAfterName(string(tree["absl/base"]), "absl_base_root_hdrs")
	if !strings.Contains(baseHdrLib, `include_prefix = "absl/base"`) {
		t.Errorf("absl_base_root_hdrs lib missing include_prefix:\n%s", baseHdrLib)
	}
	// The root-owned header (absl/meta, a non-package dir) lands in the root
	// root_hdrs lib with includes=["."] (NOT include_prefix, which would double
	// the absl/ prefix), keeping its element-root path.
	rootHdrLib := ruleBlockAfterName(string(tree[""]), "root_hdrs")
	if !strings.Contains(rootHdrLib, `includes = ["."]`) || !strings.Contains(rootHdrLib, `"absl/meta/traits.h"`) {
		t.Errorf("root_hdrs lib missing includes=[\".\"] / absl/meta/traits.h:\n%s", rootHdrLib)
	}
	if strings.Contains(rootHdrLib, "include_prefix") {
		t.Errorf("root_hdrs lib must NOT carry include_prefix:\n%s", rootHdrLib)
	}
	// Each RootInclude TARGET (the cc_library itself, not its header lib) drops
	// its walked surface, depends on the aggregate, and carries NO include_prefix
	// (that path is for the single-package glm shape only). Assert against the
	// specific rule block so the header lib in the same package can't false-pass.
	for _, tgt := range []struct{ pkg, name string }{{"absl/base", "base"}, {"absl/strings", "strings"}} {
		rule := ruleBlockAfterName(string(tree[tgt.pkg]), tgt.name)
		if rule == "" {
			t.Fatalf("no %q rule in pkg %q", tgt.name, tgt.pkg)
		}
		if !strings.Contains(rule, "//elements/x:element_root_headers") {
			t.Errorf("%s target should depend on the aggregate:\n%s", tgt.name, rule)
		}
		if strings.Contains(rule, "include_prefix") {
			t.Errorf("%s target (the consumer) must NOT carry include_prefix:\n%s", tgt.name, rule)
		}
		if strings.Contains(rule, "casts.h") || strings.Contains(rule, "traits.h") {
			t.Errorf("%s target should have dropped its walked hdr surface:\n%s", tgt.name, rule)
		}
	}
	// The fast-path must NOT drop textual_hdrs — they aren't re-homed into the
	// aggregate. base's textual_hdr survives, re-relativized to package-local.
	baseRule := ruleBlockAfterName(string(tree["absl/base"]), "base")
	if !strings.Contains(baseRule, "textual_hdrs") || !strings.Contains(baseRule, `"casts.inc"`) {
		t.Errorf("base target must KEEP its (package-local) textual_hdrs, not drop them with the re-homed surface:\n%s", baseRule)
	}
}

// TestEmit_Split_InstallExportImportSubpackage pins the zstd regression fix:
// an install(EXPORT) declarative projection emits a cc_import facade (tagged
// "cmake-codegen-install-export-import") whose static/shared_library points at
// the INSTALLED artifact path (e.g. "lib/libfoo.so"), plus a
// "cmake_config_bundle" filegroup referencing per-file write_file producers by
// ":<name>". When the artifact dir / the producers' output dir is a real
// sub-package, the split must:
//   - relabel the cc_import library path cross-package
//     (//<base>/lib:libfoo.so, not the invalid same-package "lib/libfoo.so"),
//   - relabel the filegroup's ":gen_*" srcs cross-package,
//   - publicize the re-homed producer so the root filegroup can reach it.
//
// The "manual" tag is added during LOWERING (directory_installers.go), not by
// the split; this test supplies it in the input IR and asserts EmitSplit
// preserves it on the facade (it's load-bearing for excluding the install-only
// artifact from the wildcard build / compile-db aquery). The cc_import relabel
// is also asserted under the SourceKey regime, since it's about package
// boundaries, not src/hdr framing (see the SourceKey sub-check below).
func TestEmit_Split_InstallExportImportSubpackage(t *testing.T) {
	pkg := &ir.Package{
		Name: "foo",
		Targets: []ir.Target{
			// A real library in the lib sub-package, so "lib" is a package.
			{
				Name:       "libfoo",
				Kind:       ir.KindCCLibrary,
				Srcs:       []string{"lib/foo.c"},
				Visibility: []string{"//visibility:public"},
			},
			// install(EXPORT) facade in the root, library path in the sub-package.
			{
				Name:          "libfoo_import",
				Kind:          ir.KindCCImport,
				SharedLibrary: "lib/libfoo.so",
				Tags:          []string{"cmake-codegen-install-export-import", "manual"},
				Visibility:    []string{"//visibility:public"},
			},
			// The bundle's per-file producer: output lands in the lib package.
			{
				Name:             "gen_lib_cmake_foo_fooTargets_cmake",
				Kind:             ir.KindWriteFile,
				WriteFileOut:     "lib/cmake/foo/fooTargets.cmake",
				WriteFileContent: []string{"# fooTargets"},
				Visibility:       []string{"//visibility:private"},
			},
			// The bundle filegroup in the root references the producer by :name.
			{
				Name:       "cmake_config_bundle",
				Kind:       ir.KindFilegroup,
				Srcs:       []string{":gen_lib_cmake_foo_fooTargets_cmake"},
				Visibility: []string{"//visibility:public"},
			},
		},
		SubPackages: map[string]string{"libfoo": "lib"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/foo"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	root, ok := tree[""]
	if !ok {
		t.Fatalf("no root package emitted; got dirs %v", keysOf(tree))
	}
	rb := string(root)
	// cc_import library path relabeled cross-package (not the invalid
	// same-package "lib/libfoo.so", which would be "lib is a subpackage").
	if !contains(rb, `shared_library = "//elements/foo/lib:libfoo.so"`) {
		t.Errorf("cc_import shared_library not relabeled cross-package; got:\n%s", rb)
	}
	if contains(rb, `shared_library = "lib/libfoo.so"`) {
		t.Errorf("cc_import kept the invalid same-package subpackage path; got:\n%s", rb)
	}
	// Facade tagged manual so the wildcard build / aquery skip it.
	if !contains(rb, `"manual"`) {
		t.Errorf("cc_import facade not tagged manual; got:\n%s", rb)
	}
	// Filegroup ":gen_*" src relabeled cross-package to the producer's package.
	if !contains(rb, `"//elements/foo/lib:gen_lib_cmake_foo_fooTargets_cmake"`) {
		t.Errorf("filegroup src not relabeled cross-package; got:\n%s", rb)
	}
	// The re-homed producer is publicized in the lib package (the private
	// default would be unreachable from the root filegroup).
	lib, ok := tree["lib"]
	if !ok {
		t.Fatalf("no lib package emitted; got dirs %v", keysOf(tree))
	}
	lbody := string(lib)
	if !contains(lbody, "gen_lib_cmake_foo_fooTargets_cmake") {
		t.Errorf("producer not placed in lib package; got:\n%s", lbody)
	}
	if !contains(lbody, `"//visibility:public"`) {
		t.Errorf("re-homed producer not publicized; got:\n%s", lbody)
	}

	// SourceKey regime: the cc_import library-path relabel is about emitted-BUILD
	// package boundaries, not src/hdr framing, so it must still fire (a bare
	// "lib/libfoo.so" facade in the root is just as invalid when lib/ is a
	// subpackage). srcs stay @src-prefixed in this regime, but cross-package
	// target/artifact labels still resolve to //<base>/... form.
	skTree, err := bazel.EmitSplit(pkg, bazel.Options{
		BazelPackagePath: "elements/foo",
		SourceKey:        "abc",
	})
	if err != nil {
		t.Fatalf("EmitSplit (SourceKey): %v", err)
	}
	skRoot := string(skTree[""])
	if !contains(skRoot, `shared_library = "//elements/foo/lib:libfoo.so"`) {
		t.Errorf("SourceKey: cc_import shared_library not relabeled cross-package; got:\n%s", skRoot)
	}
	if contains(skRoot, `shared_library = "lib/libfoo.so"`) {
		t.Errorf("SourceKey: cc_import kept the invalid same-package subpackage path; got:\n%s", skRoot)
	}
}

// TestEmit_Split_InSourceWorkdirGenrule_CrossPackageRefs pins the split-side
// support for in-source-generation genrules (libevent's regress.gen.c shape):
// a genrule whose output lands in a sub-package re-homes there, and its
// scratch-dir cmd references a CROSS-PACKAGE source (the generator script in the
// element root) plus an in-package input. On re-home, relocateGenruleSrcs must
// rewrite the cmd's $(execpath <src>) refs to match the relabeled srcs field
// (root script → //pkg:script cross-package label; in-package input → bare
// name), and the root package — which has no targets of its own — must still be
// emitted to host the exports_files() for the script.
func TestEmit_Split_InSourceWorkdirGenrule_CrossPackageRefs(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name:        "gen_foo",
				Kind:        ir.KindGenrule,
				Srcs:        []string{"gen.py", "sub/foo.def"},
				GenruleOuts: []string{"sub/foo.gen.c"},
				GenruleCmd: `tmp="$$(mktemp -d)" && cp "$(execpath gen.py)" "$$tmp/gen.py"` +
					` && cp "$(execpath sub/foo.def)" "$$tmp/sub/foo.def"` +
					` && cp "$$tmp/sub/foo.gen.c" "$(RULEDIR)/sub/foo.gen.c"`,
			},
			{Name: "app", Kind: ir.KindCCBinary, Srcs: []string{"sub/foo.gen.c", "sub/main.c"}},
		},
		SubPackages: map[string]string{"app": "sub"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/insrc"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	sub := string(tree["sub"])
	if sub == "" {
		t.Fatalf("no sub/ package emitted; packages = %v", keysOf(tree))
	}
	// The genrule re-homed to sub/: its cmd refs are relabeled to match the
	// relabeled srcs field (root script → cross-package label; in-package input
	// → bare; output → shrunk $(RULEDIR)).
	if !strings.Contains(sub, "$(execpath //elements/insrc:gen.py)") {
		t.Errorf("cross-package script ref not relabeled in cmd; got sub/BUILD:\n%s", sub)
	}
	if !strings.Contains(sub, "$(execpath foo.def)") || strings.Contains(sub, "$(execpath sub/foo.def)") {
		t.Errorf("in-package input ref not shrunk to package-relative; got:\n%s", sub)
	}
	if !strings.Contains(sub, "$(RULEDIR)/foo.gen.c") {
		t.Errorf("output ref not shrunk to the re-homed $(RULEDIR); got:\n%s", sub)
	}
	// The target-less root package is emitted to host exports_files(gen.py).
	root := string(tree[""])
	if !strings.Contains(root, `exports_files(["gen.py"])`) {
		t.Errorf("root package must export gen.py for the cross-package genrule ref; got root BUILD:\n%s", root)
	}
}

// TestEmit_Split_SameNameNativeRulesDistinctPackages: two recognized native
// rules sharing a name but carrying different per-target SubPackage placements
// (a/msg.proto and b/msg.proto → //a:msg_proto, //b:msg_proto) must each land in
// their OWN package — the name-keyed SubPackages map can't represent both, so
// the per-target NativeRuleSpec.SubPackage drives placement.
func TestEmit_Split_SameNameNativeRulesDistinctPackages(t *testing.T) {
	mk := func(dir string) ir.Target {
		return ir.Target{
			Name: "msg_proto", Kind: ir.KindNativeRule,
			NativeRule: &ir.NativeRuleSpec{
				Kind:       "proto_library",
				LoadFrom:   "@protobuf//bazel:proto_library.bzl",
				Attrs:      []ir.NativeAttr{{Name: "srcs", List: []string{"msg.proto"}}},
				SubPackage: dir,
			},
		}
	}
	pkg := &ir.Package{Targets: []ir.Target{mk("a"), mk("b")}}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	a, b, root := string(tree["a"]), string(tree["b"]), string(tree[""])
	if strings.Count(a, `name = "msg_proto"`) != 1 {
		t.Errorf("package a must carry exactly one msg_proto; got:\n%s", a)
	}
	if strings.Count(b, `name = "msg_proto"`) != 1 {
		t.Errorf("package b must carry exactly one msg_proto; got:\n%s", b)
	}
	if strings.Contains(root, `name = "msg_proto"`) {
		t.Errorf("root must not carry msg_proto (both are sub-packaged); got:\n%s", root)
	}
}
