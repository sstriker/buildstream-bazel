package main

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestBuildExportsDoc checks the producer-side exports manifest: each
// importable library maps to <nsPrefix><target> → //<pkgPath>:<target>,
// sorted by cmake_target, with non-library targets excluded.
func TestBuildExportsDoc(t *testing.T) {
	pkg := &ir.Package{
		Name: "greetpkg",
		Targets: []ir.Target{
			{Name: "greeter", Kind: ir.KindCCLibrary},
			{Name: "tool", Kind: ir.KindCCBinary}, // excluded — not importable
			{Name: "aux", Kind: ir.KindCCLibrary},
		},
	}
	doc := buildExportsDoc(pkg, "Greeter", "Greeter::", "elements/greetlib")
	if doc.Version != 1 || len(doc.Elements) != 1 {
		t.Fatalf("doc shape: version=%d elements=%d", doc.Version, len(doc.Elements))
	}
	el := doc.Elements[0]
	if el.Name != "Greeter" {
		t.Errorf("element name = %q, want Greeter", el.Name)
	}
	if len(el.Exports) != 2 {
		t.Fatalf("want 2 exports (libraries only), got %d", len(el.Exports))
	}
	// Sorted by cmake_target: Greeter::aux before Greeter::greeter.
	if el.Exports[0].CMakeTarget != "Greeter::aux" || el.Exports[0].BazelLabel != "//elements/greetlib:aux" {
		t.Errorf("export[0] = %+v", el.Exports[0])
	}
	if el.Exports[1].CMakeTarget != "Greeter::greeter" || el.Exports[1].BazelLabel != "//elements/greetlib:greeter" {
		t.Errorf("export[1] = %+v", el.Exports[1])
	}
}

// TestBuildExportsDoc_NoPackagePath falls back to a package-relative
// label when --bazel-package-path is absent.
func TestBuildExportsDoc_NoPackagePath(t *testing.T) {
	pkg := &ir.Package{Name: "p", Targets: []ir.Target{{Name: "p", Kind: ir.KindCCLibrary}}}
	doc := buildExportsDoc(pkg, "P", "P::", "")
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
