package main

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// TestBuildExportsDoc checks the producer-side exports manifest: each
// importable library maps to <nsPrefix><target> → //<pkgPath>:<target>,
// sorted by cmake_target, with non-library targets excluded.
func TestBuildExportsDoc(t *testing.T) {
	pkg := &ir.Package{
		Name: "greetpkg",
		Targets: []ir.Target{
			{Name: "greeter", Kind: ir.KindCCLibrary, ArtifactName: "libgreeter.a"},
			{Name: "tool", Kind: ir.KindCCBinary}, // excluded — not installed
			{Name: "aux", Kind: ir.KindCCLibrary},
		},
	}
	// An alias (Greeter::Greeter) of the real target greeter — maps to
	// greeter's label under its verbatim name.
	aliases := []cmakecfg.Alias{{Name: "Greeter::Greeter", Underlying: "greeter"}}
	doc := buildExportsDoc(pkg, "Greeter", "Greeter::", "elements/greetlib", aliases, false)
	if doc.Version != 1 || len(doc.Elements) != 1 {
		t.Fatalf("doc shape: version=%d elements=%d", doc.Version, len(doc.Elements))
	}
	el := doc.Elements[0]
	if el.Name != "Greeter" {
		t.Errorf("element name = %q, want Greeter", el.Name)
	}
	if len(el.Exports) != 3 {
		t.Fatalf("want 3 exports (2 libs + 1 alias), got %d", len(el.Exports))
	}
	// Sorted by cmake_target: Greeter::Greeter (alias), Greeter::aux,
	// Greeter::greeter. (Uppercase sorts before lowercase.)
	want := map[string]string{
		"Greeter::Greeter": "//elements/greetlib:greeter", // alias → underlying
		"Greeter::aux":     "//elements/greetlib:aux",
		"Greeter::greeter": "//elements/greetlib:greeter",
	}
	for _, ex := range el.Exports {
		if want[ex.CMakeTarget] != ex.BazelLabel {
			t.Errorf("%s = %q, want %q", ex.CMakeTarget, ex.BazelLabel, want[ex.CMakeTarget])
		}
	}
	if el.Exports[0].CMakeTarget != "Greeter::Greeter" {
		t.Errorf("not sorted: export[0] = %q, want Greeter::Greeter", el.Exports[0].CMakeTarget)
	}
	// B link keys: greeter has an ArtifactName, so it carries the link
	// name + anchored path; aux/alias (no artifact) don't.
	for _, ex := range el.Exports {
		if ex.CMakeTarget == "Greeter::greeter" {
			if len(ex.LinkLibraries) != 1 || ex.LinkLibraries[0] != "greeter" {
				t.Errorf("Greeter::greeter link_libraries = %v, want [greeter]", ex.LinkLibraries)
			}
			if len(ex.LinkPaths) != 1 || ex.LinkPaths[0] != "/opt/prefix/lib/libgreeter.a" {
				t.Errorf("Greeter::greeter link_paths = %v, want [/opt/prefix/lib/libgreeter.a]", ex.LinkPaths)
			}
		}
		if ex.CMakeTarget == "Greeter::aux" && len(ex.LinkLibraries) != 0 {
			t.Errorf("aux (no artifact) should have no link_libraries; got %v", ex.LinkLibraries)
		}
	}
}

// TestBuildExportsDoc_DeclarativeBundleLabels checks the Phase 6
// resolved-lift manifest-synth (M3): when the lowered IR carries a
// declarative install(EXPORT) projection (a "cmake_config_bundle"
// filegroup plus tagged `<lib>_import` cc_import facades), every
// emitted Export carries the absolute bundle label + the sorted list
// of import-facade labels so a cross-element find_package consumer can
// resolve straight to them.
func TestBuildExportsDoc_DeclarativeBundleLabels(t *testing.T) {
	const tag = "cmake-codegen-install-export-import"
	pkg := &ir.Package{
		Name: "foopkg",
		Targets: []ir.Target{
			{Name: "foo", Kind: ir.KindCCLibrary, ArtifactName: "libfoo.a"},
			{Name: "bar", Kind: ir.KindCCLibrary, ArtifactName: "libbar.so.1"},
			// Phase 6 declarative projection: per-target import
			// facades (tagged) + the package-wide bundle filegroup.
			{Name: "bar_import", Kind: ir.KindCCImport, Tags: []string{tag}},
			{Name: "foo_import", Kind: ir.KindCCImport, Tags: []string{tag}},
			{Name: "cmake_config_bundle", Kind: ir.KindFilegroup,
				Srcs: []string{"lib/cmake/foopkg/foopkgTargets.cmake"}},
		},
	}
	doc := buildExportsDoc(pkg, "foopkg", "FooPkg::", "elements/components/foopkg", nil, false)
	wantBundle := "//elements/components/foopkg:cmake_config_bundle"
	wantImports := []string{
		"//elements/components/foopkg:bar_import",
		"//elements/components/foopkg:foo_import",
	}
	for _, ex := range doc.Elements[0].Exports {
		if ex.CMakeConfigBundleLabel != wantBundle {
			t.Errorf("%s CMakeConfigBundleLabel = %q, want %q", ex.CMakeTarget, ex.CMakeConfigBundleLabel, wantBundle)
		}
		if !reflect.DeepEqual(ex.CMakeImportLabels, wantImports) {
			t.Errorf("%s CMakeImportLabels = %v, want %v", ex.CMakeTarget, ex.CMakeImportLabels, wantImports)
		}
	}
}

// TestBuildExportsDoc_NoBundleOmitsLabels confirms a non-bundle
// element (no cmake_config_bundle in the IR) leaves both Phase 6
// fields empty, so its exports.json stays byte-identical to the
// pre-Phase-6 shape.
func TestBuildExportsDoc_NoBundleOmitsLabels(t *testing.T) {
	pkg := &ir.Package{
		Name: "plain",
		Targets: []ir.Target{
			{Name: "plain", Kind: ir.KindCCLibrary, ArtifactName: "libplain.a"},
		},
	}
	doc := buildExportsDoc(pkg, "Plain", "Plain::", "elements/plain", nil, false)
	for _, ex := range doc.Elements[0].Exports {
		if ex.CMakeConfigBundleLabel != "" {
			t.Errorf("%s CMakeConfigBundleLabel = %q, want empty", ex.CMakeTarget, ex.CMakeConfigBundleLabel)
		}
		if ex.CMakeImportLabels != nil {
			t.Errorf("%s CMakeImportLabels = %v, want nil", ex.CMakeTarget, ex.CMakeImportLabels)
		}
	}
}

func TestLinkLibName(t *testing.T) {
	cases := map[string]string{
		"libz.so":       "z",
		"libgreeter.a":  "greeter",
		"libz.so.1.3.1": "z",
		"libfoo.dylib":  "foo",
		"greeter.a":     "", // no lib prefix
		"lib.so":        "", // empty name
		"":              "",
	}
	for in, want := range cases {
		if got := linkLibName(in); got != want {
			t.Errorf("linkLibName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildExportsDoc_NoPackagePath falls back to a package-relative
// label when --bazel-package-path is absent.
func TestBuildExportsDoc_NoPackagePath(t *testing.T) {
	pkg := &ir.Package{Name: "p", Targets: []ir.Target{{Name: "p", Kind: ir.KindCCLibrary}}}
	doc := buildExportsDoc(pkg, "P", "P::", "", nil, false)
	if got := doc.Elements[0].Exports[0].BazelLabel; got != ":p" {
		t.Errorf("label = %q, want :p", got)
	}
}

// TestExportNamespaceForPackage covers the trace-driven recovery of
// the install(EXPORT ... NAMESPACE ...) prefix the codemodel drops.
// The headline case — project name != export namespace (zlib-shaped:
// project "zlib", NAMESPACE "ZLIB::") — is the one no existing
// sample fixture exercises and the reason the recovery exists.
func TestExportNamespaceForPackage(t *testing.T) {
	const (
		// project "zlib" but the export namespace is "ZLIB::".
		zlibShaped = `{"args":["EXPORT","zlibTargets","FILE","zlibTargets.cmake","NAMESPACE","ZLIB::","DESTINATION","lib/cmake/zlib"],"cmd":"install","file":"/src/CMakeLists.txt","line":40}` + "\n"
		// Two exports; pick the one whose DESTINATION matches the pkg.
		twoExports = `{"args":["EXPORT","fooTargets","NAMESPACE","Foo::","DESTINATION","lib/cmake/foo"],"cmd":"install","file":"/src/CMakeLists.txt","line":1}` + "\n" +
			`{"args":["EXPORT","barTargets","NAMESPACE","Bar::","DESTINATION","lib/cmake/bar"],"cmd":"install","file":"/src/CMakeLists.txt","line":2}` + "\n"
	)
	tests := []struct {
		name    string
		trace   string
		pkgName string
		want    string
	}{
		{"namespace differs from project name", zlibShaped, "zlib", "ZLIB::"},
		{"destination-matched export wins", twoExports, "bar", "Bar::"},
		{"no namespace-bearing export", `{"args":["TARGETS","z","EXPORT","zTargets","DESTINATION","lib"],"cmd":"install","file":"/src/CMakeLists.txt","line":1}` + "\n", "z", ""},
		{"empty trace", "", "z", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exportNamespaceForPackage([]byte(tt.trace), tt.pkgName); got != tt.want {
				t.Errorf("exportNamespaceForPackage(%q) = %q, want %q", tt.pkgName, got, tt.want)
			}
		})
	}
}

// TestBuildExportsDoc_InstalledExecutable: an installed executable's
// export row carries the namespaced CMakeTarget, its label, and the
// anchored bin/ path in link_paths — the key the consumer-side genrule
// tool lift (rewriteToolFromTarget) matches when a custom command
// drives the tool by its prefix-resolved path. No link_libraries (-l
// semantics don't apply to executables). Non-installed executables are
// excluded entirely (no prefix-reachable path exists).
func TestBuildExportsDoc_InstalledExecutable(t *testing.T) {
	pkg := &ir.Package{
		Name: "toolpkg",
		Targets: []ir.Target{
			{Name: "gen", Kind: ir.KindCCBinary, ArtifactName: "gen", InstallDest: "bin"},
			{Name: "scratch", Kind: ir.KindCCBinary, ArtifactName: "scratch"},
		},
	}
	doc := buildExportsDoc(pkg, "toolpkg", "Tool::", "elements/toolpkg", nil, false)
	if len(doc.Elements) != 1 {
		t.Fatalf("elements = %d", len(doc.Elements))
	}
	var gen *manifest.Export
	for _, ex := range doc.Elements[0].Exports {
		if ex.CMakeTarget == "Tool::gen" {
			gen = ex
		}
		if ex.CMakeTarget == "Tool::scratch" {
			t.Errorf("non-installed executable must not export: %+v", ex)
		}
	}
	if gen == nil {
		t.Fatalf("installed executable missing from exports: %+v", doc.Elements[0].Exports)
	}
	if gen.BazelLabel != "//elements/toolpkg:gen" {
		t.Errorf("BazelLabel = %q", gen.BazelLabel)
	}
	if len(gen.LinkPaths) != 1 || gen.LinkPaths[0] != lower.ManifestPrefixAnchor+"bin/gen" {
		t.Errorf("LinkPaths = %v, want the anchored bin/ path", gen.LinkPaths)
	}
	if len(gen.LinkLibraries) != 0 {
		t.Errorf("executables carry no -l semantics; LinkLibraries = %v", gen.LinkLibraries)
	}
}

// TestBuildExportsDoc_NoDepsForConvertedElements pins the Export.Deps
// INVARIANT on the producer side: converted elements' export rows
// leave Deps EMPTY — BazelLabel is a real rule whose own deps Bazel
// resolves, and filling Deps would double-wire every consumer with
// direct edges to the export's internals (the over-emit shape the
// link attribution's trace-gated drop exists to avoid). Deps is the
// UNMODELED closure: hand-written prebuilt-backed manifests only.
func TestBuildExportsDoc_NoDepsForConvertedElements(t *testing.T) {
	pkg := &ir.Package{
		Name: "greetpkg",
		Targets: []ir.Target{
			{Name: "core", Kind: ir.KindCCLibrary, ArtifactName: "libcore.a",
				Deps: []string{":base", "@abseil-cpp//absl/strings:strings"}},
			{Name: "base", Kind: ir.KindCCLibrary, ArtifactName: "libbase.a"},
		},
	}
	doc := buildExportsDoc(pkg, "greetpkg", "Greeter::", "elements/greetlib", nil, false)
	for _, ex := range doc.Elements[0].Exports {
		if len(ex.Deps) != 0 {
			t.Errorf("%s: producer-emitted Deps must stay empty (invariant: Deps = unmodeled closure); got %v", ex.CMakeTarget, ex.Deps)
		}
	}
}
