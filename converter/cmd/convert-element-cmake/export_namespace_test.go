package main

import "testing"

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
