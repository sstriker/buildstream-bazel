package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
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

// TestSplitCompileFragments_DropsCudaArchFlags: cmake bakes
// CMAKE_CUDA_ARCHITECTURES into `--generate-code=arch=...` compile flags, but
// rules_cuda rejects per-target arch flags in copts (arch is a toolchain/flag
// concern). splitCompileFragments drops both the `=`-joined and the
// space-separated nvcc arch-flag forms while keeping every other copt.
func TestSplitCompileFragments_DropsCudaArchFlags(t *testing.T) {
	frags := []fileapi.CommandFragment{
		{Fragment: "-O3"},
		{Fragment: "--generate-code=arch=compute_75,code=[compute_75,sm_75]"},
		{Fragment: `"--generate-code=arch=compute_120,code=[compute_120,sm_120]"`},
		{Fragment: "-gencode=arch=compute_80,code=sm_80"},
		{Fragment: "--extended-lambda"},
		{Fragment: "-gencode arch=compute_86,code=sm_86"},
		{Fragment: "--generate-code arch=compute_90,code=sm_90"},
		{Fragment: "-std=c++17"},
	}
	copts, _, _ := splitCompileFragments(frags)
	want := []string{"-O3", "--extended-lambda", "-std=c++17"}
	if len(copts) != len(want) {
		t.Fatalf("copts = %v; want %v", copts, want)
	}
	for i, c := range copts {
		if c != want[i] {
			t.Fatalf("copts = %v; want %v", copts, want)
		}
	}
}

// TestSplitCompileFragments_ShellEscapedDefine pins the shell-tokenized
// extraction of a string-valued define: OpenBLAS's CMAKE_C_FLAGS carries
// `-DVERSION="\"0.3.28\""` (the macro value being the C string "0.3.28"). A
// naive whitespace split leaves the embedded `\"` escapes, which Bazel then
// passes verbatim to gcc ("missing terminating \" character"). Shell tokenizing
// collapses them to the faithful argv token, so the emitted define is
// `VERSION="0.3.28"` (the inner quotes preserved, the shell escaping gone).
func TestSplitCompileFragments_ShellEscapedDefine(t *testing.T) {
	frags := []fileapi.CommandFragment{
		{Fragment: `-DVERSION="\"0.3.28\""`},
		{Fragment: "-DNDEBUG"},
		{Fragment: `-DMSG="hello world"`}, // quoted value with a space stays one define
	}
	_, defines, _ := splitCompileFragments(frags)
	want := []string{`VERSION="0.3.28"`, "NDEBUG", "MSG=hello world"}
	if len(defines) != len(want) {
		t.Fatalf("defines = %q; want %q", defines, want)
	}
	for i, d := range defines {
		if d != want[i] {
			t.Fatalf("defines[%d] = %q; want %q (full: %q)", i, d, want[i], defines)
		}
	}
}

// TestRetagCudaTargets_PerPlatformSrcs: a platform-guarded `.cu` executable
// (cuda-samples' systemWideAtomics: `if(Linux) add_executable(x x.cu)`)
// folds its ONLY compiled src into a PerPlatform srcs select arm, leaving
// flat Srcs empty — the retag must see the arms or the target stays a
// cc_binary whose nvcc-flag copts fail under gcc. A mixed arm (a C TU in
// another platform's arm) still blocks the retag.
func TestRetagCudaTargets_PerPlatformSrcs(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{
			Name: "guarded",
			Kind: ir.KindCCBinary,
			PerPlatform: map[string]map[string][]string{
				"srcs": {"@platforms//os:linux": {"systemWideAtomics.cu"}},
			},
		},
		{
			Name: "mixedarms",
			Kind: ir.KindCCBinary,
			PerPlatform: map[string]map[string][]string{
				"srcs": {
					"@platforms//os:linux":  {"k.cu"},
					"@platforms//os:darwin": {"host.c"},
				},
			},
		},
	}}
	retagCudaTargets(pkg)

	if got := findTarget(pkg, "guarded"); got == nil || got.Kind != ir.KindCudaBinary {
		t.Errorf("guarded Kind = %v; want KindCudaBinary (per-platform .cu must retag)", got.Kind)
	}
	if got := findTarget(pkg, "mixedarms"); got == nil || got.Kind != ir.KindCCBinary {
		t.Errorf("mixedarms Kind = %v; want KindCCBinary (mixed arms must not retag)", got.Kind)
	}
}
