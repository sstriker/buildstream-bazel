package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestDropGenexIncludeDirs covers the glog finding: an unresolved
// generator-expression include dir (`$<TARGET_PROPERTY:…>`) is dropped from a
// target's Includes (it's not a real path), while real include dirs are kept
// in order.
func TestDropGenexIncludeDirs(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "mixed", Kind: ir.KindCCLibrary, Includes: []string{"include", "$<TARGET_PROPERTY:glog,INCLUDE_DIRECTORIES>", "src"}},
		{Name: "only_genex", Kind: ir.KindCCLibrary, Includes: []string{"$<TARGET_PROPERTY:foo,INCLUDE_DIRECTORIES>"}},
		{Name: "clean", Kind: ir.KindCCLibrary, Includes: []string{"include"}},
	}}

	if n := dropGenexIncludeDirs(pkg); n != 2 {
		t.Errorf("dropped = %d, want 2", n)
	}
	// Mixed: the genex entry is removed, the real ones kept in order.
	if got := pkg.Targets[0].Includes; len(got) != 2 || got[0] != "include" || got[1] != "src" {
		t.Errorf("mixed.Includes = %v, want [include src]", got)
	}
	// Only-genex: the slice is emptied (nil).
	if pkg.Targets[1].Includes != nil {
		t.Errorf("only_genex.Includes = %v, want nil", pkg.Targets[1].Includes)
	}
	// Clean: untouched.
	if got := pkg.Targets[2].Includes; len(got) != 1 || got[0] != "include" {
		t.Errorf("clean.Includes = %v, want [include]", got)
	}
}

// TestDropGenexIncludeDirs_NoGenex is the no-op guard: with no genex include
// dirs nothing is dropped and the slices are left unchanged.
func TestDropGenexIncludeDirs_NoGenex(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, Includes: []string{"include", "src/util"}},
	}}
	if n := dropGenexIncludeDirs(pkg); n != 0 {
		t.Errorf("dropped = %d, want 0", n)
	}
	if got := pkg.Targets[0].Includes; len(got) != 2 || got[0] != "include" || got[1] != "src/util" {
		t.Errorf("Includes changed: %v", got)
	}
}
