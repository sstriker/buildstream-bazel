package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestSplitFortranModuleSrcs_UseColonColonOrdering: a module provider whose
// `use` of another provider's module is written in the F2003 `use :: M` form
// (and the `use, non_intrinsic :: M` form) is still ordered after its provider.
// Guards fortranUseRe against the double-colon forms.
func TestSplitFortranModuleSrcs_UseColonColonOrdering(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// base defines M (no deps); mid defines N, `use :: M`; top defines O,
	// `use, non_intrinsic :: N`. Expected provider order: base, mid, top.
	write("base.f90", "module m\n integer :: a\nend module m\n")
	write("mid.f90", "module n\n use :: m\n integer :: b\nend module n\n")
	write("top.f90", "module o\n use, non_intrinsic :: n\n integer :: c\nend module o\n")
	write("plain.f", "      subroutine sub\n      end\n") // no module → rest

	srcs := []string{"top.f90", "mid.f90", "base.f90", "plain.f"}
	mod, rest := splitFortranModuleSrcs(srcs, dir)

	if !fpEqual(mod, []string{"base.f90", "mid.f90", "top.f90"}) {
		t.Errorf("module order = %v; want [base.f90 mid.f90 top.f90] (provider before user)", mod)
	}
	if !fpEqual(rest, []string{"plain.f"}) {
		t.Errorf("rest = %v; want [plain.f]", rest)
	}
}

func fpEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fpContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func findTarget(pkg *ir.Package, name string) *ir.Target {
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == name {
			return &pkg.Targets[i]
		}
	}
	return nil
}

// TestRetagFortranTargets_MixedTarget: a cc_library with both C and Fortran
// srcs keeps its C srcs (stays a cc_library) and the Fortran srcs move to a
// private sibling <name>_fortran fortran_library the cc_library deps on; both
// carry the cmake-codegen-fortran-target tag.
func TestRetagFortranTargets_MixedTarget(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "blas",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"wrap.c", "dgemm.f", "ddot.f90", "helper.cc"},
	}}}
	retagFortranTargets(pkg, "")

	cc := findTarget(pkg, "blas")
	if cc == nil {
		t.Fatal("cc target blas dropped (should survive — it has C srcs)")
	}
	if cc.Kind != ir.KindCCLibrary {
		t.Errorf("blas Kind = %v; want KindCCLibrary (keeps its C srcs)", cc.Kind)
	}
	if !fpEqual(cc.Srcs, []string{"wrap.c", "helper.cc"}) {
		t.Errorf("blas srcs = %v; want [wrap.c helper.cc] (Fortran moved out)", cc.Srcs)
	}
	if !fpContains(cc.Deps, ":blas_fortran") {
		t.Errorf("blas should dep on :blas_fortran; got deps %v", cc.Deps)
	}
	if !fpContains(cc.Tags, "cmake-codegen-fortran-target") {
		t.Errorf("blas should be tagged cmake-codegen-fortran-target; got %v", cc.Tags)
	}
	fl := findTarget(pkg, "blas_fortran")
	if fl == nil {
		t.Fatal("blas_fortran fortran_library not created")
	}
	if fl.Kind != ir.KindFortranLibrary {
		t.Errorf("sibling Kind = %v; want KindFortranLibrary", fl.Kind)
	}
	if !fpEqual(fl.Srcs, []string{"dgemm.f", "ddot.f90"}) {
		t.Errorf("fortran_library srcs = %v; want [dgemm.f ddot.f90]", fl.Srcs)
	}
	if !fpContains(fl.Visibility, "//visibility:private") {
		t.Errorf("sibling should be private; got %v", fl.Visibility)
	}
	if !fpContains(fl.Tags, "cmake-codegen-fortran-target") {
		t.Errorf("sibling should carry the tag; got %v", fl.Tags)
	}
}

// TestRetagFortranTargets_FortranOnlyRetaggedInPlace: a cc_library whose srcs
// are ALL Fortran (OpenBLAS's reference-LAPACK shape) is retagged IN PLACE to a
// fortran_library, keeping its name so existing deps edges still resolve.
func TestRetagFortranTargets_FortranOnlyRetaggedInPlace(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:               "lapack_ref",
		Kind:               ir.KindCCLibrary,
		Srcs:               []string{"dlamch.f", "ilaver.f"},
		Defines:            []string{"ADD_"},
		ImplementationDeps: []string{":blas"},
	}}}
	retagFortranTargets(pkg, "")

	fl := findTarget(pkg, "lapack_ref")
	if fl == nil {
		t.Fatal("Fortran-only target should be retagged in place, not dropped")
	}
	if fl.Kind != ir.KindFortranLibrary {
		t.Errorf("lapack_ref Kind = %v; want KindFortranLibrary", fl.Kind)
	}
	if !fpEqual(fl.Srcs, []string{"dlamch.f", "ilaver.f"}) {
		t.Errorf("srcs = %v; want both .f files", fl.Srcs)
	}
	// defines fold into -D copts; implementation_deps fold into deps.
	if !fpContains(fl.Copts, "-DADD_") {
		t.Errorf("define should fold to -DADD_ copt; got copts %v", fl.Copts)
	}
	if len(fl.Defines) != 0 {
		t.Errorf("defines should be cleared after folding; got %v", fl.Defines)
	}
	if !fpContains(fl.Deps, ":blas") {
		t.Errorf("implementation_deps should fold into deps; got %v", fl.Deps)
	}
	if len(fl.ImplementationDeps) != 0 {
		t.Errorf("implementation_deps should be cleared; got %v", fl.ImplementationDeps)
	}
	if !fpContains(fl.Tags, "cmake-codegen-fortran-target") {
		t.Errorf("retagged target should carry the tag; got %v", fl.Tags)
	}
}

// TestRetagFortranTargets_NoFortranUnchanged: a pure-C/C++ target is left
// byte-for-byte unchanged (no sibling, no tag, no retag) — the pass is a no-op
// for the common case.
func TestRetagFortranTargets_NoFortranUnchanged(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "lib",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"a.c", "b.cc"},
	}}}
	retagFortranTargets(pkg, "")

	if len(pkg.Targets) != 1 {
		t.Fatalf("expected 1 target (no sibling); got %d", len(pkg.Targets))
	}
	cc := pkg.Targets[0]
	if cc.Kind != ir.KindCCLibrary {
		t.Errorf("Kind changed: %v", cc.Kind)
	}
	if !fpEqual(cc.Srcs, []string{"a.c", "b.cc"}) {
		t.Errorf("srcs changed: %v", cc.Srcs)
	}
	if len(cc.Tags) != 0 {
		t.Errorf("no tag expected for pure-C target; got %v", cc.Tags)
	}
}
