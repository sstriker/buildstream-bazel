package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
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
	retagCudaTargets(pkg, nil)

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
	retagCudaTargets(pkg, nil)

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
	retagCudaTargets(pkg, nil)

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
	retagCudaTargets(pkg, nil)

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
	retagCudaTargets(pkg, nil)

	if got := findTarget(pkg, "guarded"); got == nil || got.Kind != ir.KindCudaBinary {
		t.Errorf("guarded Kind = %v; want KindCudaBinary (per-platform .cu must retag)", got.Kind)
	}
	if got := findTarget(pkg, "mixedarms"); got == nil || got.Kind != ir.KindCCBinary {
		t.Errorf("mixedarms Kind = %v; want KindCCBinary (mixed arms must not retag)", got.Kind)
	}
}

// TestCudaRdcTargetNames: the ninja device-link edge
// (CMakeFiles/<target>.dir[/<Config>]/cmake_device_link.o) is the only
// artifact of CUDA_SEPARABLE_COMPILATION the convert can see — the File
// API exposes no property/fragment for it. Both the single- and
// multi-config object-dir layouts must resolve to the cmake target name.
func TestCudaRdcTargetNames(t *testing.T) {
	g := &ninja.Graph{Builds: []*ninja.Build{
		{Outputs: []string{"CMakeFiles/cdpSimplePrint.dir/cmake_device_link.o"}},
		{Outputs: []string{"cpp/3_CUDA_Features/cdpQuadtree/CMakeFiles/cdpQuadtree.dir/Release/cmake_device_link.o"}},
		{Outputs: []string{"CMakeFiles/plain.dir/plain.cu.o"}}, // not a device link
	}}
	got := cudaRdcTargetNames(g)
	if !got["cdpSimplePrint"] || !got["cdpQuadtree"] {
		t.Errorf("device-link targets missed: %v", got)
	}
	if got["plain"] {
		t.Errorf("non-device-link target wrongly marked: %v", got)
	}
	if cudaRdcTargetNames(nil) != nil {
		t.Error("nil graph must yield nil set")
	}
}

// TestRetagCudaTargets_Rdc: a separable-compilation target (per the ninja
// device-link set) carries CudaRdc onto the retagged rule; its sibling
// without the edge does not.
func TestRetagCudaTargets_Rdc(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "cdp", Kind: ir.KindCCBinary, Srcs: []string{"cdp.cu"}},
		{Name: "plain", Kind: ir.KindCCBinary, Srcs: []string{"plain.cu"}},
	}}
	retagCudaTargets(pkg, map[string]bool{"cdp": true})
	if got := findTarget(pkg, "cdp"); got == nil || got.Kind != ir.KindCudaBinary || !got.CudaRdc {
		t.Errorf("cdp = kind %v rdc %v; want cuda_binary with CudaRdc", got.Kind, got.CudaRdc)
	}
	if got := findTarget(pkg, "plain"); got == nil || got.CudaRdc {
		t.Errorf("plain must not carry CudaRdc")
	}
}

// TestCollectDirHeaders_Cuh: a kernel .cuh beside its .cu (the cuda-samples
// `<sample>_kernel.cuh` quote-include shape) must stage like any sibling
// header — the walk previously skipped the extension and the include missed
// in Bazel's sandbox.
func TestCollectDirHeaders_Cuh(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"k_kernel.cuh", "helper.h", "main.cu", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("#pragma once\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := collectDirHeaders(root, root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"k_kernel.cuh": true, "helper.h": true}
	for _, h := range got {
		if !want[h] {
			t.Errorf("unexpected walk entry %q", h)
		}
		delete(want, h)
	}
	for missing := range want {
		t.Errorf("walk missed %q", missing)
	}
}

// TestPartitionCudaLinkopts: a retagged CUDA target's link line splits per
// rules_cuda's model — host-shaped flags (-l/-Wl,/-fopenmp) land in
// HostLinkOpts (plain `linkopts` is the DEVICE link there, and the binary
// macros drop it from the host link entirely: cudaOpenMP's missing -lgomp),
// the toolchain-owned CUDA runtime libs drop, and nvcc driver leftovers
// (-lineinfo, --extended-lambda) drop rather than riding the device link.
// Config-select arms partition the same way.
func TestPartitionCudaLinkopts(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "omp",
		Kind: ir.KindCCBinary,
		Srcs: []string{"omp.cu"},
		LinkOpts: []string{
			"-lineinfo", "--extended-lambda",
			"-Wl,-rpath,/usr/lib/gcc/x86_64-linux-gnu/13",
			"-lgomp", "-lpthread", "-lcudadevrt", "-lcudart_static", "-lrt",
		},
		PerPlatform: map[string]map[string][]string{
			"linkopts": {"//config:debug": {"-lgomp_dbg", "-lineinfo"}},
		},
	}}}
	retagCudaTargets(pkg, nil)
	got := findTarget(pkg, "omp")
	if got.Kind != ir.KindCudaBinary {
		t.Fatalf("omp Kind = %v; want KindCudaBinary", got.Kind)
	}
	wantHost := []string{"-Wl,-rpath,/usr/lib/gcc/x86_64-linux-gnu/13", "-lgomp", "-lpthread", "-lrt"}
	if len(got.HostLinkOpts) != len(wantHost) {
		t.Fatalf("HostLinkOpts = %v; want %v", got.HostLinkOpts, wantHost)
	}
	for i := range wantHost {
		if got.HostLinkOpts[i] != wantHost[i] {
			t.Fatalf("HostLinkOpts = %v; want %v", got.HostLinkOpts, wantHost)
		}
	}
	if got.LinkOpts != nil {
		t.Errorf("device linkopts must clear; got %v", got.LinkOpts)
	}
	if arms := got.PerPlatform["host_linkopts"]; len(arms["//config:debug"]) != 1 || arms["//config:debug"][0] != "-lgomp_dbg" {
		t.Errorf("config arm partition = %v", arms)
	}
	if _, stale := got.PerPlatform["linkopts"]; stale {
		t.Errorf("stale linkopts arms survived")
	}
}

// TestRetagCudaTargets_RdcPlacement: a MIXED-language separable target's
// device-link edge must mark the split's CUDA sub-library
// (<cmake-target>_cuda), never the cc wrapper — cc_binary has no `rdc`
// attribute and rendering it there fails analysis ("no such attribute").
func TestRetagCudaTargets_RdcPlacement(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "mergeSort", Kind: ir.KindCCBinary, Srcs: []string{"host.cpp"}},
		{Name: "mergeSort_cuda", Kind: ir.KindCudaLibrary, Srcs: []string{"mergeSort.cu"}},
	}}
	retagCudaTargets(pkg, map[string]bool{"mergeSort": true})
	if got := findTarget(pkg, "mergeSort"); got == nil || got.CudaRdc || got.Kind != ir.KindCCBinary {
		t.Errorf("cc wrapper must stay cc_binary without CudaRdc: kind=%v rdc=%v", got.Kind, got.CudaRdc)
	}
	if got := findTarget(pkg, "mergeSort_cuda"); got == nil || !got.CudaRdc {
		t.Errorf("split CUDA sub-library must carry CudaRdc")
	}
}
