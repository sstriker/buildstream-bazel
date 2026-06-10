package bazel_test

import (
	"strings"
	"testing"

	bazel "github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_FortranLibrary_LoadAndAttrs: a KindFortranLibrary renders as
// fortran_library from @rules_buildstream_bazel//rules:fortran.bzl, carries the
// attributes the rule accepts (srcs/deps/copts/linkopts/includes), and drops
// every cc-only attribute (hdrs/textual_hdrs/defines/local_defines/
// implementation_deps/include_prefix/linkstatic/alwayslink/features) that the
// rule would reject at analysis.
func TestEmit_FortranLibrary_LoadAndAttrs(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:     "lapack_ref",
		Kind:     ir.KindFortranLibrary,
		Srcs:     []string{"dlamch.f", "ilaver.f90"},
		Includes: []string{"include"},
		Copts:    []string{"-frecursive", "-DADD_"},
		LinkOpts: []string{"-Wl,--no-as-needed"},
		Deps:     []string{":blas"},
		// cc-only fields that must NOT render on fortran_library.
		Hdrs:               []string{"k.h"},
		TextualHdrs:        []string{"inl.fi"},
		Defines:            []string{"SHOULD_NOT_RENDER=1"},
		LocalDefines:       []string{"ALSO_NOT=1"},
		ImplementationDeps: []string{":impl"},
		IncludePrefix:      "lapack",
		StripIncludePrefix: "include",
		Features:           []string{"pic"},
		Linkstatic:         true,
		Alwayslink:         true,
	}}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, `load("@rules_buildstream_bazel//rules:fortran.bzl", "fortran_library")`) {
		t.Errorf("missing fortran_library load; got:\n%s", got)
	}
	if !strings.Contains(got, "fortran_library(") {
		t.Errorf("expected fortran_library rule; got:\n%s", got)
	}
	// Accepted attributes pass through.
	for _, want := range []string{`"dlamch.f"`, `"ilaver.f90"`, `"-frecursive"`, `"-DADD_"`, `"-Wl,--no-as-needed"`, `":blas"`, `"include"`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %s in fortran_library; got:\n%s", want, got)
		}
	}
	// cc-only attributes must be dropped (would hard-fail analysis on the rule).
	for _, banned := range []string{"hdrs", "textual_hdrs", "defines", "local_defines", "implementation_deps", "include_prefix", "strip_include_prefix", "linkstatic", "alwayslink", "features"} {
		if strings.Contains(got, banned+" =") {
			t.Errorf("%s must be dropped on fortran_library; got:\n%s", banned, got)
		}
	}
}

// TestEmit_NoFortran_NoFortranLoad: a pure cc package emits no fortran.bzl load.
func TestEmit_NoFortran_NoFortranLoad(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "lib",
		Kind: ir.KindCCLibrary,
		Srcs: []string{"a.c"},
	}}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(string(out), "fortran") {
		t.Errorf("non-Fortran package must not load fortran.bzl; got:\n%s", out)
	}
}
