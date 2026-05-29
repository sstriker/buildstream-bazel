package main

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestBuildExportsDoc checks the producer-side exports manifest: each
// importable library maps to <nsPrefix><target> → //<pkgPath>:<target>,
// sorted by cmake_target, with non-library targets excluded.
func TestBuildExportsDoc(t *testing.T) {
	pkg := &ir.Package{
		Name: "greetpkg",
		Targets: []ir.Target{
			{Name: "greeter", Kind: ir.KindCCLibrary, ArtifactName: "libgreeter.a"},
			{Name: "tool", Kind: ir.KindCCBinary}, // excluded — not importable
			{Name: "aux", Kind: ir.KindCCLibrary},
		},
	}
	// An alias (Greeter::Greeter) of the real target greeter — maps to
	// greeter's label under its verbatim name.
	aliases := []cmakecfg.Alias{{Name: "Greeter::Greeter", Underlying: "greeter"}}
	doc := buildExportsDoc(pkg, "Greeter", "Greeter::", "elements/greetlib", aliases)
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
	doc := buildExportsDoc(pkg, "P", "P::", "", nil)
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
