package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestDisambiguateTestNameCollisions covers the OpenBLAS shape: a
// cc_test synthesized from a malformed add_test() takes a name that
// collides with a real (codemodel-derived) target. The authoritative
// target keeps its name; the cc_test is renamed and tagged, so the
// emitted package has unique names and Bazel doesn't reject it.
func TestDisambiguateTestNameCollisions(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		// Authoritative add_executable target (emitted first).
		{Name: "openblas_utest_ext", Kind: ir.KindCCBinary},
		// The real utest binary's malformed-add_test cc_test, named
		// after the OTHER target.
		{Name: "openblas_utest_ext", Kind: ir.KindCCTest},
	}}
	disambiguateTestNameCollisions(pkg)

	if pkg.Targets[0].Name != "openblas_utest_ext" || pkg.Targets[0].Kind != ir.KindCCBinary {
		t.Errorf("authoritative target should keep its name; got %+v", pkg.Targets[0])
	}
	if pkg.Targets[1].Name != "openblas_utest_ext_test" {
		t.Errorf("colliding cc_test should be renamed to openblas_utest_ext_test; got %q", pkg.Targets[1].Name)
	}
	if !stringSliceContains(pkg.Targets[1].Tags, "cmake-test-name-disambiguated") {
		t.Errorf("renamed cc_test should carry the disambiguation tag; got tags %v", pkg.Targets[1].Tags)
	}
}

// TestDisambiguateTestNameCollisions_NoCollisionNoChange is the
// no-regression guard: distinct names are left byte-for-byte unchanged
// (no spurious renames or tags).
func TestDisambiguateTestNameCollisions_NoCollisionNoChange(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "lib", Kind: ir.KindCCLibrary},
		{Name: "bin", Kind: ir.KindCCBinary},
		{Name: "the_test", Kind: ir.KindCCTest},
	}}
	disambiguateTestNameCollisions(pkg)
	want := []string{"lib", "bin", "the_test"}
	for i, w := range want {
		if pkg.Targets[i].Name != w {
			t.Errorf("target %d renamed unexpectedly: got %q want %q", i, pkg.Targets[i].Name, w)
		}
		if len(pkg.Targets[i].Tags) != 0 {
			t.Errorf("target %d got spurious tags %v", i, pkg.Targets[i].Tags)
		}
	}
}

// TestDisambiguateTestNameCollisions_NonTestDuplicateLeftForValidate
// confirms a duplicate that ISN'T a cc_test is left alone — those are
// real bugs the bazelconstraints validate pass should still surface
// (renaming a library/binary could break a label reference).
func TestDisambiguateTestNameCollisions_NonTestDuplicateLeftForValidate(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "dup", Kind: ir.KindCCLibrary},
		{Name: "dup", Kind: ir.KindCCLibrary},
	}}
	disambiguateTestNameCollisions(pkg)
	if pkg.Targets[1].Name != "dup" {
		t.Errorf("non-test duplicate should be left for the validate pass; got %q", pkg.Targets[1].Name)
	}
}
