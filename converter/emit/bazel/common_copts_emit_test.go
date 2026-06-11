package bazel_test

import (
	"strings"
	"testing"

	bazel "github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_CommonCoptsConstant: a package with CommonCoptsLabel set and targets
// marked PrependCommonCopts emits the load() once and renders each target's
// copts as `COMMON_COPTS` / `COMMON_COPTS + [delta]`.
func TestEmit_CommonCoptsConstant(t *testing.T) {
	pkg := &ir.Package{
		CommonCoptsLabel: "//elements/p:common_compile_flags.bzl",
		Targets: []ir.Target{
			{Name: "a", Kind: ir.KindCCLibrary, Srcs: []string{"a.c"}, PrependCommonCopts: true},
			{Name: "b", Kind: ir.KindCCLibrary, Srcs: []string{"b.c"}, Copts: []string{"-Wall"}, PrependCommonCopts: true},
		},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, `load("//elements/p:common_compile_flags.bzl", "COMMON_COPTS")`) {
		t.Errorf("missing COMMON_COPTS load; got:\n%s", got)
	}
	if strings.Count(got, `"COMMON_COPTS")`) != 1 {
		t.Errorf("expected exactly one COMMON_COPTS load; got:\n%s", got)
	}
	if !strings.Contains(got, "copts = COMMON_COPTS,") {
		t.Errorf("target a should render copts = COMMON_COPTS; got:\n%s", got)
	}
	if !strings.Contains(got, `copts = COMMON_COPTS + ["-Wall"]`) {
		t.Errorf("target b should render COMMON_COPTS + [delta]; got:\n%s", got)
	}
}

// TestEmit_NoCommonCoptsConstant_NoLoad: without CommonCoptsLabel, no load and
// no COMMON_COPTS reference leak into a plain package.
func TestEmit_NoCommonCoptsConstant_NoLoad(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, Srcs: []string{"a.c"}, Copts: []string{"-O2"}},
	}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(string(out), "COMMON_COPTS") {
		t.Errorf("plain package must not reference COMMON_COPTS; got:\n%s", out)
	}
}
