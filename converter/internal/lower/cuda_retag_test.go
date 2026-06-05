package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestRetagCudaTargets_PureCudaLibrary: a cc_library whose only compiled srcs
// are `.cu` retags to cuda_library (rules_cuda compiles it via nvcc; a
// cc_library can't). Non-.cu headers in srcs/hdrs don't block the retag.
func TestRetagCudaTargets_PureCudaLibrary(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "kernels",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"a.cu", "b.cu"},
		Hdrs: []string{"k.cuh", "k.h"},
	}}}
	retagCudaTargets(pkg)

	got := findTarget(pkg, "kernels")
	if got == nil {
		t.Fatal("kernels target vanished")
	}
	if got.Kind != ir.KindCudaLibrary {
		t.Errorf("kernels Kind = %v; want KindCudaLibrary", got.Kind)
	}
	if got.Kind.String() != "cuda_library" {
		t.Errorf("kernels rule name = %q; want cuda_library", got.Kind.String())
	}
}

// TestRetagCudaTargets_CudaBinaryAndTest: cc_binary / cc_test of `.cu` retag
// to cuda_binary / cuda_test.
func TestRetagCudaTargets_CudaBinaryAndTest(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "app", Kind: ir.KindCCBinary, Srcs: []string{"main.cu"}},
		{Name: "t", Kind: ir.KindCCTest, Srcs: []string{"t.cu"}},
	}}
	retagCudaTargets(pkg)

	if app := findTarget(pkg, "app"); app == nil || app.Kind != ir.KindCudaBinary {
		t.Errorf("app Kind = %v; want KindCudaBinary", app.Kind)
	}
	if tt := findTarget(pkg, "t"); tt == nil || tt.Kind != ir.KindCudaTest {
		t.Errorf("t Kind = %v; want KindCudaTest", tt.Kind)
	}
}

// TestRetagCudaTargets_MixedStaysCC: a target mixing `.cu` with a C/C++
// compiled src is NOT retagged — retagging to cuda_* would drop the C/C++ TU
// from the cc compile path. (In practice the per-language split handles mixed
// targets before this pass; this guards the defensive leftover case.)
func TestRetagCudaTargets_MixedStaysCC(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "mixed",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"k.cu", "host.cc"},
	}}}
	retagCudaTargets(pkg)

	if got := findTarget(pkg, "mixed"); got == nil || got.Kind != ir.KindCCLibrary {
		t.Errorf("mixed Kind = %v; want KindCCLibrary (not retagged)", got.Kind)
	}
}

// TestRetagCudaTargets_NoCudaUnchanged: a pure-C/C++ target is untouched.
func TestRetagCudaTargets_NoCudaUnchanged(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "lib",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"a.c", "b.cc"},
	}}}
	retagCudaTargets(pkg)

	if got := findTarget(pkg, "lib"); got == nil || got.Kind != ir.KindCCLibrary {
		t.Errorf("lib Kind = %v; want unchanged KindCCLibrary", got.Kind)
	}
}

// TestStripBalancedQuotes: cmake shell-quotes a CUDA `--generate-code` fragment
// for the `[`/`,`; Bazel's no-shell argv needs it bare. A balanced surrounding
// pair is stripped; unbalanced / quote-less tokens are untouched.
func TestStripBalancedQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"--generate-code=arch=compute_80,code=[sm_80]"`, `--generate-code=arch=compute_80,code=[sm_80]`},
		{`-O3`, `-O3`},
		{`"unterminated`, `"unterminated`},
		{`trailing"`, `trailing"`},
		{`""`, ``},
		{`"`, `"`},
	}
	for _, c := range cases {
		if got := stripBalancedQuotes(c.in); got != c.want {
			t.Errorf("stripBalancedQuotes(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
