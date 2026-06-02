package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/bazelconstraints"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestSanitizeTestName covers the char-level mapping into the
// bazelconstraints identifier subset: `:` (and anything else outside
// [A-Za-z0-9_.+-]) becomes `_`, mid-name `.`/`+`/`-` survive, and a
// leading `.`/`+`/`-` (legal only after the first char) is mapped to `_`.
func TestSanitizeTestName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Benchmarking::FailureReporting::FailMacro", "Benchmarking__FailureReporting__FailMacro"},
		{"List::Tests::Output", "List__Tests__Output"},
		{"a:b", "a_b"},
		{"already_valid.name-1+x", "already_valid.name-1+x"},
		{"-leading-dash", "_leading-dash"},
		{".dotfirst", "_dotfirst"},
		{"plain", "plain"},
		{"", "unnamed_test"},
	}
	for _, c := range cases {
		if got := sanitizeTestName(c.in); got != c.want {
			t.Errorf("sanitizeTestName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeTestNames_ResolvesCtestAbort is the issue #368 regression:
// a cc_test carrying a CTest `Suite::Case` name (Catch2 / GoogleTest
// convention) makes the package fail the bazelconstraints validate pass,
// which aborts the whole convert. After sanitizeTestNames the package
// validates clean, while the library/binary targets the operator wants
// are left untouched.
func TestSanitizeTestNames_ResolvesCtestAbort(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "SelfTest", Kind: ir.KindCCBinary},
		{Name: "Catch2", Kind: ir.KindCCLibrary},
		{Name: "Benchmarking::FailureReporting::FailMacro", Kind: ir.KindCCTest},
		{Name: "List::Tests::Output", Kind: ir.KindCCTest},
	}}
	// Precondition: the raw colon names fail validation — this is the abort.
	if err := bazelconstraints.ValidatePackage(pkg); err == nil {
		t.Fatal("expected raw CTest colon names to fail validation (the abort precondition)")
	}

	sanitizeTestNames(pkg)

	if err := bazelconstraints.ValidatePackage(pkg); err != nil {
		t.Errorf("package still invalid after sanitize: %v", err)
	}
	// Non-test targets untouched (no rename, no spurious tag).
	if pkg.Targets[0].Name != "SelfTest" || pkg.Targets[1].Name != "Catch2" {
		t.Errorf("non-test targets renamed: %q, %q", pkg.Targets[0].Name, pkg.Targets[1].Name)
	}
	if len(pkg.Targets[1].Tags) != 0 {
		t.Errorf("library got spurious tags %v", pkg.Targets[1].Tags)
	}
	// cc_tests sanitized + tagged.
	if pkg.Targets[2].Name != "Benchmarking__FailureReporting__FailMacro" {
		t.Errorf("cc_test not sanitized: %q", pkg.Targets[2].Name)
	}
	if !stringSliceContains(pkg.Targets[2].Tags, "cmake-test-name-sanitized") {
		t.Errorf("sanitized cc_test missing tag; got %v", pkg.Targets[2].Tags)
	}
	for _, tt := range pkg.Targets {
		if strings.Contains(tt.Name, ":") {
			t.Errorf("colon remains in name %q", tt.Name)
		}
	}
}

// TestSanitizeTestNames_ThenDisambiguate covers a rewrite that folds two
// distinct CTest names into the same identifier: sanitizeTestNames runs
// first, then disambiguateTestNameCollisions must split the collision so
// the emitted package still has unique, valid names.
func TestSanitizeTestNames_ThenDisambiguate(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "A_B", Kind: ir.KindCCTest},
		{Name: "A:B", Kind: ir.KindCCTest}, // sanitizes to A_B → collides with the first
	}}
	sanitizeTestNames(pkg)
	disambiguateTestNameCollisions(pkg)

	if err := bazelconstraints.ValidatePackage(pkg); err != nil {
		t.Errorf("sanitize+disambiguate left an invalid/duplicate package: %v", err)
	}
	if pkg.Targets[0].Name != "A_B" {
		t.Errorf("first cc_test should keep A_B; got %q", pkg.Targets[0].Name)
	}
	if pkg.Targets[1].Name != "A_B_test" {
		t.Errorf("collided cc_test should be disambiguated to A_B_test; got %q", pkg.Targets[1].Name)
	}
}

// TestSanitizeTestNames_EmptyNames guards the empty-NAME edge: ctest.Parse
// doesn't reject an add_test() with an empty NAME, and validNameRe rejects
// "" — so the sanitizer maps it to a placeholder and the duplicate is
// split by the disambiguate pass, leaving a valid package.
func TestSanitizeTestNames_EmptyNames(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "", Kind: ir.KindCCTest},
		{Name: "", Kind: ir.KindCCTest},
	}}
	sanitizeTestNames(pkg)
	disambiguateTestNameCollisions(pkg)

	if err := bazelconstraints.ValidatePackage(pkg); err != nil {
		t.Errorf("empty-named cc_tests still invalid after sanitize+disambiguate: %v", err)
	}
	if pkg.Targets[0].Name != "unnamed_test" || pkg.Targets[1].Name != "unnamed_test_test" {
		t.Errorf("empty names not handled: %q, %q", pkg.Targets[0].Name, pkg.Targets[1].Name)
	}
}
