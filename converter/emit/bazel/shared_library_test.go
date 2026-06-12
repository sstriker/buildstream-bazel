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

// TestEmit_DynamicDeps confirms a consumer's DynamicDeps render as the
// dynamic_deps attribute (faithful-SHARED Phase 2).
func TestEmit_DynamicDeps(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "app", Kind: ir.KindCCBinary, Srcs: []string{"main.c"},
		Deps: []string{":z"}, DynamicDeps: []string{":z_shared"},
	}}}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(string(got), `dynamic_deps = [":z_shared"]`) {
		t.Errorf("missing dynamic_deps:\n%s", got)
	}
}

// TestEmit_DynamicDepsKindGate: `dynamic_deps` exists only on the linking
// rules (cc_binary/cc_test) — a cc_library carrying DynamicDeps (lower's
// wireDynamicDeps now kind-gates, but defensively) must NOT render the attr;
// libevent's static-variant libraries were the canary ("no such attribute
// 'dynamic_deps' in 'cc_library' rule" took the whole package down).
func TestEmit_DynamicDepsKindGate(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "consumer_lib", Kind: ir.KindCCLibrary, Srcs: []string{"a.c"}, DynamicDeps: []string{":impl_shared"}},
		{Name: "consumer_bin", Kind: ir.KindCCBinary, Srcs: []string{"m.c"}, DynamicDeps: []string{":impl_shared"}},
	}}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)
	if n := strings.Count(got, "dynamic_deps"); n != 1 {
		t.Errorf("dynamic_deps must render exactly once (the cc_binary); got %d in:\n%s", n, got)
	}
}
