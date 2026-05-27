package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestStripDepOwnedHdrs_StripsOwnedHdrsWhenDepDeclared pins the
// boost-shape: consumer's hdrs lose entries owned by a sibling
// that's already in deps. Bazel propagates hdrs through deps so
// the consumer still sees them at compile time.
func TestStripDepOwnedHdrs_StripsOwnedHdrsWhenDepDeclared(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{
			Name: "boost_config",
			Kind: ir.KindCCLibrary,
			Hdrs: []string{"include/boost/config.hpp"},
		},
		{
			Name: "boost_atomic_cxx_0",
			Kind: ir.KindCCLibrary,
			Hdrs: []string{
				"include/boost/atomic/atomic.hpp",
				"include/boost/config.hpp", // owned by boost_config, in deps
			},
			Deps: []string{":boost_config"},
		},
	}}
	stripDepOwnedHdrs(pkg)
	atomicHdrs := pkg.Targets[1].Hdrs
	want := []string{"include/boost/atomic/atomic.hpp"}
	if !reflect.DeepEqual(atomicHdrs, want) {
		t.Errorf("atomic hdrs = %v, want %v", atomicHdrs, want)
	}
	// boost_config's own hdrs must stay (it owns them).
	if got, want := pkg.Targets[0].Hdrs, []string{"include/boost/config.hpp"}; !reflect.DeepEqual(got, want) {
		t.Errorf("boost_config hdrs mutated; got %v, want %v", got, want)
	}
}

// TestStripDepOwnedHdrs_PreservesWhenNoDep covers the conservative
// guard: if the consumer doesn't declare the owning sibling in deps,
// the hdr stays — stripping would break consumers that compiled
// against the header without the explicit dep (a latent cmake
// declaration bug we don't want to surface during the cutover).
func TestStripDepOwnedHdrs_PreservesWhenNoDep(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "owner", Kind: ir.KindCCLibrary, Hdrs: []string{"owner.h"}},
		{
			Name: "consumer",
			Kind: ir.KindCCLibrary,
			Hdrs: []string{"owner.h"}, // owner not in deps -> keep
		},
	}}
	stripDepOwnedHdrs(pkg)
	if got, want := pkg.Targets[1].Hdrs, []string{"owner.h"}; !reflect.DeepEqual(got, want) {
		t.Errorf("consumer hdrs = %v, want %v (preserved when no dep)", got, want)
	}
}

// TestStripDepOwnedHdrs_ImplementationDepsCount: PRIVATE deps
// (implementation_deps) also propagate hdrs in the consumer's
// compile action — the strip must consider both lists.
func TestStripDepOwnedHdrs_ImplementationDepsCount(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "owner", Kind: ir.KindCCLibrary, Hdrs: []string{"o.h"}},
		{
			Name:               "consumer",
			Kind:               ir.KindCCLibrary,
			Hdrs:               []string{"o.h", "c.h"},
			ImplementationDeps: []string{":owner"},
		},
	}}
	stripDepOwnedHdrs(pkg)
	want := []string{"c.h"}
	if got := pkg.Targets[1].Hdrs; !reflect.DeepEqual(got, want) {
		t.Errorf("consumer hdrs = %v, want %v", got, want)
	}
}

// TestStripDepOwnedHdrs_CrossPackageDepsIgnored: deps that
// reference a cross-package label can't strip an in-package hdr
// (the owning target isn't a sibling).
func TestStripDepOwnedHdrs_CrossPackageDepsIgnored(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "owner", Kind: ir.KindCCLibrary, Hdrs: []string{"o.h"}},
		{
			Name: "consumer",
			Kind: ir.KindCCLibrary,
			Hdrs: []string{"o.h"},
			Deps: []string{"//elsewhere:owner"}, // cross-package, not a sibling
		},
	}}
	stripDepOwnedHdrs(pkg)
	if got, want := pkg.Targets[1].Hdrs, []string{"o.h"}; !reflect.DeepEqual(got, want) {
		t.Errorf("consumer hdrs = %v, want %v (cross-package dep doesn't authorize strip)", got, want)
	}
}

// TestStripDepOwnedHdrs_OwnerKeepsItsOwnHdrs: the owning target
// itself doesn't lose hdrs (it owns them, can't strip).
func TestStripDepOwnedHdrs_OwnerKeepsItsOwnHdrs(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "lib", Kind: ir.KindCCLibrary, Hdrs: []string{"a.h", "b.h"}},
	}}
	stripDepOwnedHdrs(pkg)
	if got, want := pkg.Targets[0].Hdrs, []string{"a.h", "b.h"}; !reflect.DeepEqual(got, want) {
		t.Errorf("owner hdrs = %v, want %v", got, want)
	}
}

// TestStripDepOwnedHdrs_NilSafe: nil + empty package don't panic.
func TestStripDepOwnedHdrs_NilSafe(t *testing.T) {
	stripDepOwnedHdrs(nil)
	stripDepOwnedHdrs(&ir.Package{})
}
