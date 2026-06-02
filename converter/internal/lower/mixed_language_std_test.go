package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestCompileGroupMixesCAndCXX pins issue #315's detection: a CompileGroup
// whose sources include BOTH a C and a C++ file (by extension) is mixed, so
// the caller skips prepending the group's language `-std=` flag (which would
// leak onto the other language's sources). Headers and single-language
// groups are not mixed.
func TestCompileGroupMixesCAndCXX(t *testing.T) {
	srcs := []fileapi.TargetSource{
		{Path: "impl.c"},        // 0: C
		{Path: "wrapper.cpp"},   // 1: CXX
		{Path: "api.h"},         // 2: header (neutral)
		{Path: "extra.cc"},      // 3: CXX
		{Path: "gen/thing.cxx"}, // 4: CXX
		{Path: "asm/boot.s"},    // 5: neutral (non-C/C++)
		{Path: "legacy.c++"},    // 6: CXX
		{Path: "upper.C"},       // 7: CXX (uppercase .C is C++ by convention)
	}
	cases := []struct {
		name string
		idx  []int
		want bool
	}{
		{"C + C++ → mixed", []int{0, 1}, true},
		{"C + C++ with header → mixed", []int{0, 1, 2}, true},
		{"C-only → not mixed", []int{0, 2}, false},
		{"lowercase .c + uppercase .C → mixed (.C is C++)", []int{0, 7}, true},
		{"uppercase .C only → not mixed (all C++)", []int{7}, false},
		{"C++-only (several exts) → not mixed", []int{1, 3, 4, 6}, false},
		{"header-only → not mixed", []int{2}, false},
		{"C + asm (non-C/C++) → not mixed", []int{0, 5}, false},
		{"out-of-range indexes ignored", []int{0, 99, -1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cg := fileapi.CompileGroup{SourceIndexes: c.idx}
			if got := compileGroupMixesCAndCXX(cg, srcs); got != c.want {
				t.Errorf("compileGroupMixesCAndCXX = %v; want %v", got, c.want)
			}
		})
	}
}
