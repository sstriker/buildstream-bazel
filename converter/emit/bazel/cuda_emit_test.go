package bazel_test

import (
	"strings"
	"testing"

	bazel "github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_CudaLibrary_LoadAndAttrs: a KindCudaLibrary renders as cuda_library
// from @rules_cuda//cuda:defs.bzl, and the attributes rules_cuda's cuda_library
// does NOT accept are adapted — textual_hdrs folds into hdrs,
// implementation_deps folds into deps, and include_prefix / strip_include_prefix
// / linkstatic / features are dropped (emitting any of them hard-fails analysis
// with "no attribute X"). copts/defines/includes pass through.
func TestEmit_CudaLibrary_LoadAndAttrs(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:               "kernels",
		Kind:               ir.KindCudaLibrary,
		Srcs:               []string{"k.cu"},
		Hdrs:               []string{"k.h"},
		TextualHdrs:        []string{"inl.cuh"},
		Includes:           []string{"include"},
		Copts:              []string{"--expt-relaxed-constexpr"},
		Defines:            []string{"USE_CUDA=1"},
		Deps:               []string{":base"},
		ImplementationDeps: []string{":impl"},
		IncludePrefix:      "kernels",
		StripIncludePrefix: "include",
		Features:           []string{"pic"},
		Linkstatic:         true,
	}}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, `load("@rules_cuda//cuda:defs.bzl", "cuda_library")`) {
		t.Errorf("missing rules_cuda load; got:\n%s", got)
	}
	if !strings.Contains(got, "cuda_library(") {
		t.Errorf("expected cuda_library rule; got:\n%s", got)
	}
	// textual_hdrs folded into hdrs (no textual_hdrs attribute on cuda_library).
	if strings.Contains(got, "textual_hdrs") {
		t.Errorf("textual_hdrs should be folded into hdrs, not emitted; got:\n%s", got)
	}
	if !strings.Contains(got, `"inl.cuh"`) || !strings.Contains(got, `"k.h"`) {
		t.Errorf("hdrs should carry both k.h and the folded inl.cuh; got:\n%s", got)
	}
	// implementation_deps folded into deps.
	if strings.Contains(got, "implementation_deps") {
		t.Errorf("implementation_deps should fold into deps, not emit; got:\n%s", got)
	}
	if !strings.Contains(got, `":impl"`) || !strings.Contains(got, `":base"`) {
		t.Errorf("deps should carry both :base and the folded :impl; got:\n%s", got)
	}
	// Dropped attributes (unsupported on cuda_library).
	for _, banned := range []string{"include_prefix", "strip_include_prefix", "linkstatic", "features"} {
		if strings.Contains(got, banned) {
			t.Errorf("%s must be dropped on cuda_library (unsupported); got:\n%s", banned, got)
		}
	}
	// Pass-through attributes.
	for _, kept := range []string{"--expt-relaxed-constexpr", "USE_CUDA=1", `"include"`} {
		if !strings.Contains(got, kept) {
			t.Errorf("expected %q to survive; got:\n%s", kept, got)
		}
	}
}

// TestEmit_CudaBinary_FoldsHdrsToSrcs: cuda_binary (like cc_binary) has no hdrs
// attribute, so headers fold into srcs; the @rules_cuda load names cuda_binary.
func TestEmit_CudaBinary_FoldsHdrsToSrcs(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "app",
		Kind: ir.KindCudaBinary,
		Srcs: []string{"main.cu"},
		Hdrs: []string{"app.h"},
		Deps: []string{":kernels"},
	}}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, `load("@rules_cuda//cuda:defs.bzl", "cuda_binary")`) {
		t.Errorf("missing cuda_binary load; got:\n%s", got)
	}
	if strings.Contains(got, "hdrs") {
		t.Errorf("cuda_binary has no hdrs; app.h should fold into srcs; got:\n%s", got)
	}
	if !strings.Contains(got, `"app.h"`) || !strings.Contains(got, `"main.cu"`) {
		t.Errorf("srcs should carry main.cu + folded app.h; got:\n%s", got)
	}
}

// TestEmit_NoCuda_NoCudaLoad: a pure cc package emits no @rules_cuda load
// (byte-stability for the non-CUDA common case).
func TestEmit_NoCuda_NoCudaLoad(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "lib",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"a.cc"},
	}}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(string(out), "rules_cuda") {
		t.Errorf("non-CUDA package must not load rules_cuda; got:\n%s", out)
	}
}

// TestEmit_CudaRdc: CudaRdc renders rules_cuda's `rdc = True` (forwarded by
// the cuda_binary macro to its inner cuda_library); unset emits no attr.
func TestEmit_CudaRdc(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "cdp", Kind: ir.KindCudaBinary, Srcs: []string{"cdp.cu"}, CudaRdc: true},
		{Name: "plain", Kind: ir.KindCudaBinary, Srcs: []string{"plain.cu"}},
	}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "rdc = True") {
		t.Errorf("missing rdc = True for separable-compilation target; got:\n%s", got)
	}
	if strings.Count(got, "rdc = True") != 1 {
		t.Errorf("rdc must render only on the marked target; got:\n%s", got)
	}
}

// TestEmit_CudaHostLinkopts: HostLinkOpts renders as rules_cuda's
// `host_linkopts` (the macro renames it onto the outer cc_binary's
// linkopts; a plain `linkopts` would be silently dropped from the host
// link).
func TestEmit_CudaHostLinkopts(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:         "omp",
		Kind:         ir.KindCudaBinary,
		Srcs:         []string{"omp.cu"},
		HostLinkOpts: []string{"-lgomp"},
	}}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "host_linkopts = [\"-lgomp\"]") {
		t.Errorf("missing host_linkopts; got:\n%s", got)
	}
	if strings.Contains(got, "\n    linkopts") {
		t.Errorf("plain linkopts must not render for the partitioned target; got:\n%s", got)
	}
}
