package bazel_test

import (
	"strings"
	"testing"

	bazel "github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_SharedLibrary renders a SHARED_LIBRARY impl carrying SharedLibName
// and asserts emit adds the sibling cc_shared_library wrapper + its load.
func TestEmit_SharedLibrary(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:          "z",
		Kind:          ir.KindCCLibrary,
		Srcs:          []string{"adler32.c"},
		SharedLibName: "libz.so",
	}}}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		`load("@rules_cc//cc:defs.bzl", "cc_library", "cc_shared_library")`,
		`cc_shared_library(`,
		`name = "z_shared"`,
		`shared_lib_name = "libz.so"`,
		`deps = [":z"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("emitted BUILD missing %q\n---\n%s", want, s)
		}
	}
}

// TestEmit_SharedLibrary_OffByDefault confirms a plain cc_library (no
// SharedLibName) emits no cc_shared_library — the static-collapse default.
func TestEmit_SharedLibrary_OffByDefault(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "z", Kind: ir.KindCCLibrary, Srcs: []string{"adler32.c"},
	}}}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if strings.Contains(string(got), "cc_shared_library") {
		t.Errorf("unexpected cc_shared_library for a plain cc_library:\n%s", got)
	}
}
