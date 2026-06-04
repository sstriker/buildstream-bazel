package exportshape

import "testing"

// TestPkgFromBundle covers the config-mode namespace derivation across the
// install(EXPORT) destination layouts Classify accepts: a <Pkg> segment in the
// dest (preferred) and the no-<Pkg> destinations that fall back to the export
// name (with a trailing "Targets" stripped).
func TestPkgFromBundle(t *testing.T) {
	cases := []struct{ dest, exportName, want string }{
		{"lib/cmake/GTest", "GTestTargets", "GTest"},   // canonical: <Pkg> is the last component
		{"share/MyPkg/cmake", "MyPkgTargets", "MyPkg"}, // share/<Pkg>/cmake
		{"lib/cmake/absl", "absl", "absl"},             // <Pkg> present; export name has no "Targets" suffix
		{"lib/cmake", "GTestTargets", "GTest"},         // no <Pkg> segment → export-name fallback
		{"share/cmake", "FooTargets", "Foo"},           // no <Pkg> segment → export-name fallback
	}
	for _, tc := range cases {
		if got := pkgFromBundle(tc.dest, tc.exportName); got != tc.want {
			t.Errorf("pkgFromBundle(%q, %q) = %q, want %q", tc.dest, tc.exportName, got, tc.want)
		}
	}
}
