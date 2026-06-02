package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestPropagateOpenMPLinkFlag pins issue #313: a `-fopenmp` compile flag is
// mirrored onto linkopts (gcc/clang need it at link time to pull in the
// OpenMP runtime), without duplicating one already there, and the exact
// flag form is preserved.
func TestPropagateOpenMPLinkFlag(t *testing.T) {
	cases := []struct {
		name      string
		copts     []string
		linkOpts  []string
		wantLinks []string
	}{
		{"mirrors -fopenmp onto linkopts", []string{"-O2", "-fopenmp"}, nil, []string{"-fopenmp"}},
		{"preserves -fopenmp=libomp form", []string{"-fopenmp=libomp"}, nil, []string{"-fopenmp=libomp"}},
		{"no dup when already in linkopts", []string{"-fopenmp"}, []string{"-fopenmp"}, []string{"-fopenmp"}},
		{"no-op without openmp copt", []string{"-O2", "-Wall"}, nil, nil},
		{"appends, keeping existing linkopts", []string{"-fopenmp"}, []string{"-lm"}, []string{"-lm", "-fopenmp"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tg := &ir.Target{Copts: c.copts, LinkOpts: c.linkOpts}
			propagateOpenMPLinkFlag(tg)
			if !reflect.DeepEqual(tg.LinkOpts, c.wantLinks) {
				t.Errorf("LinkOpts = %v; want %v", tg.LinkOpts, c.wantLinks)
			}
		})
	}
}
