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
