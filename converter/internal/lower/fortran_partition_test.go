package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

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

// TestPartitionFortranSources_MixedTarget: a cc_library with both C and
// Fortran srcs keeps its C srcs (stays buildable) and the Fortran srcs
// move to a sibling <name>_fortran_srcs filegroup; both carry the
// cmake-codegen-fortran-target tag.
func TestPartitionFortranSources_MixedTarget(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "blas",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"wrap.c", "dgemm.f", "ddot.f90", "helper.cc"},
	}}}
	partitionFortranSources(pkg)

	cc := findTarget(pkg, "blas")
	if cc == nil {
		t.Fatal("cc target blas dropped (should survive — it has C srcs)")
	}
	if !fpEqual(cc.Srcs, []string{"wrap.c", "helper.cc"}) {
		t.Errorf("blas srcs = %v; want [wrap.c helper.cc] (Fortran moved out)", cc.Srcs)
	}
	if !fpContains(cc.Tags, "cmake-codegen-fortran-target") {
		t.Errorf("blas should be tagged cmake-codegen-fortran-target; got %v", cc.Tags)
	}
	fg := findTarget(pkg, "blas_fortran_srcs")
	if fg == nil {
		t.Fatal("blas_fortran_srcs filegroup not created")
	}
	if fg.Kind != ir.KindFilegroup {
		t.Errorf("filegroup Kind = %v; want KindFilegroup", fg.Kind)
	}
	if !fpEqual(fg.Srcs, []string{"dgemm.f", "ddot.f90"}) {
		t.Errorf("filegroup srcs = %v; want [dgemm.f ddot.f90]", fg.Srcs)
	}
	if !fpContains(fg.Tags, "cmake-codegen-fortran-target") {
		t.Errorf("filegroup should carry the tag; got %v", fg.Tags)
	}
}

// TestPartitionFortranSources_FortranOnlyTargetDropped: a cc_library
// whose srcs are ALL Fortran (OpenBLAS's reference-LAPACK shape) would
// be srcs-less after partitioning (Bazel-invalid), so the cc target is
// dropped and the filegroup carries everything.
func TestPartitionFortranSources_FortranOnlyTargetDropped(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "lapack_ref",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"dlamch.f", "ilaver.f"},
	}}}
	partitionFortranSources(pkg)

	if cc := findTarget(pkg, "lapack_ref"); cc != nil {
		t.Errorf("Fortran-only cc target should be dropped; still present with srcs %v", cc.Srcs)
	}
	fg := findTarget(pkg, "lapack_ref_fortran_srcs")
	if fg == nil || fg.Kind != ir.KindFilegroup {
		t.Fatalf("expected lapack_ref_fortran_srcs filegroup; got %v", fg)
	}
	if !fpEqual(fg.Srcs, []string{"dlamch.f", "ilaver.f"}) {
		t.Errorf("filegroup srcs = %v; want both .f files", fg.Srcs)
	}
}

// TestPartitionFortranSources_NoFortranUnchanged: a pure-C/C++ target is
// left byte-for-byte unchanged (no filegroup, no tag) — the partition is
// a no-op for the common case.
func TestPartitionFortranSources_NoFortranUnchanged(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "lib",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"a.c", "b.cc"},
	}}}
	partitionFortranSources(pkg)

	if len(pkg.Targets) != 1 {
		t.Fatalf("expected 1 target (no filegroup); got %d", len(pkg.Targets))
	}
	cc := pkg.Targets[0]
	if !fpEqual(cc.Srcs, []string{"a.c", "b.cc"}) {
		t.Errorf("srcs changed: %v", cc.Srcs)
	}
	if len(cc.Tags) != 0 {
		t.Errorf("no tag expected for pure-C target; got %v", cc.Tags)
	}
}
