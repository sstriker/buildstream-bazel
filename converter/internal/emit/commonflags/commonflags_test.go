package commonflags

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func has(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// TestHoistCommonCopts_PrefixStrippedAndTagged: the leading flags every cc
// target shares are hoisted (returned), stripped from each target's copts, and
// the targets are tagged with the feature; the per-target delta is preserved in
// order.
func TestHoistCommonCopts_PrefixStrippedAndTagged(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, Copts: []string{"-O2", "-mavx2", "-DA"}},
		{Name: "b", Kind: ir.KindCCBinary, Copts: []string{"-O2", "-mavx2", "-DB", "-Wall"}},
		{Name: "c", Kind: ir.KindCCLibrary, Copts: []string{"-O2", "-mavx2"}},
	}}
	got := HoistCommonCopts(pkg, FeatureName)
	if !eq(got, []string{"-O2", "-mavx2"}) {
		t.Fatalf("hoisted = %v; want [-O2 -mavx2]", got)
	}
	want := map[string][]string{
		"a": {"-DA"},
		"b": {"-DB", "-Wall"},
		"c": nil,
	}
	for i := range pkg.Targets {
		tg := pkg.Targets[i]
		if !eq(tg.Copts, want[tg.Name]) {
			t.Errorf("%s copts = %v; want %v", tg.Name, tg.Copts, want[tg.Name])
		}
		if !has(tg.Features, FeatureName) {
			t.Errorf("%s not tagged with %q; features=%v", tg.Name, FeatureName, tg.Features)
		}
	}
}

// TestHoistCommonCopts_NoSharedPrefix: when targets share no leading flag,
// nothing is hoisted and copts are untouched.
func TestHoistCommonCopts_NoSharedPrefix(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, Copts: []string{"-O2", "-DA"}},
		{Name: "b", Kind: ir.KindCCLibrary, Copts: []string{"-O3", "-DB"}},
	}}
	if got := HoistCommonCopts(pkg, FeatureName); got != nil {
		t.Fatalf("hoisted = %v; want nil (no shared prefix)", got)
	}
	if !eq(pkg.Targets[0].Copts, []string{"-O2", "-DA"}) || has(pkg.Targets[0].Features, FeatureName) {
		t.Errorf("target a mutated unexpectedly: copts=%v features=%v", pkg.Targets[0].Copts, pkg.Targets[0].Features)
	}
}

// TestHoistCommonCopts_EmptyCoptsSkipped: a cc target with no copts neither
// shrinks the shared prefix nor gets tagged; non-cc targets are ignored.
func TestHoistCommonCopts_EmptyCoptsSkipped(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, Copts: []string{"-O2", "-mavx2", "-DA"}},
		{Name: "b", Kind: ir.KindCCLibrary, Copts: []string{"-O2", "-mavx2", "-DB"}},
		{Name: "hdr", Kind: ir.KindCCInterface}, // no copts → skipped
		{Name: "gen", Kind: ir.KindGenrule, Copts: []string{"-O2", "-mavx2"}},
	}}
	got := HoistCommonCopts(pkg, FeatureName)
	if !eq(got, []string{"-O2", "-mavx2"}) {
		t.Fatalf("hoisted = %v; want [-O2 -mavx2] (empty-copts + non-cc don't cap it)", got)
	}
	if has(pkg.Targets[2].Features, FeatureName) {
		t.Errorf("empty-copts target should not be tagged")
	}
	if has(pkg.Targets[3].Features, FeatureName) {
		t.Errorf("non-cc target should not be tagged")
	}
}

// TestHoistCommonCopts_NeedsTwoCandidates: a single cc target with copts is not
// enough to hoist (nothing to dedup against).
func TestHoistCommonCopts_NeedsTwoCandidates(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, Copts: []string{"-O2", "-mavx2"}},
		{Name: "hdr", Kind: ir.KindCCInterface},
	}}
	if got := HoistCommonCopts(pkg, FeatureName); got != nil {
		t.Fatalf("hoisted = %v; want nil (need >= 2 candidates)", got)
	}
}

// TestHoistCommonCoptsToConstant_StripAndMark: the self-contained mode strips
// the shared prefix, marks each target PrependCommonCopts, and records the load
// label on the package (no features tag).
func TestHoistCommonCoptsToConstant_StripAndMark(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, Copts: []string{"-O2", "-mavx2", "-DA"}},
		{Name: "b", Kind: ir.KindCCBinary, Copts: []string{"-O2", "-mavx2"}},
	}}
	got := HoistCommonCoptsToConstant(pkg, "//pkg:common_compile_flags.bzl")
	if !eq(got, []string{"-O2", "-mavx2"}) {
		t.Fatalf("hoisted = %v; want [-O2 -mavx2]", got)
	}
	if pkg.CommonCoptsLabel != "//pkg:common_compile_flags.bzl" {
		t.Errorf("CommonCoptsLabel = %q; want the load label", pkg.CommonCoptsLabel)
	}
	if !eq(pkg.Targets[0].Copts, []string{"-DA"}) || !pkg.Targets[0].PrependCommonCopts {
		t.Errorf("a: copts=%v prepend=%v; want [-DA] true", pkg.Targets[0].Copts, pkg.Targets[0].PrependCommonCopts)
	}
	if pkg.Targets[1].Copts != nil || !pkg.Targets[1].PrependCommonCopts {
		t.Errorf("b: copts=%v prepend=%v; want nil true", pkg.Targets[1].Copts, pkg.Targets[1].PrependCommonCopts)
	}
	// Must NOT also tag the feature (that's the other mode).
	if has(pkg.Targets[0].Features, FeatureName) {
		t.Errorf("constant mode should not tag the toolchain feature")
	}
}

// TestEmitConstant_Shape: the constant .bzl defines COMMON_COPTS; empty input
// still emits a valid empty list.
func TestEmitConstant_Shape(t *testing.T) {
	out := string(EmitConstant([]string{"-O2", "-mavx2"}))
	for _, want := range []string{"COMMON_COPTS = [", `"-O2"`, `"-mavx2"`} {
		if !strings.Contains(out, want) {
			t.Errorf("EmitConstant missing %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(string(EmitConstant(nil)), "COMMON_COPTS = []") {
		t.Errorf("empty EmitConstant should emit an empty list")
	}
}

// TestEmit_FeatureShape: the rendered .bzl loads the toolchain lib, defines the
// feature carrying the copts (enabled = False), and exports the list. Empty
// input still emits a valid empty export.
func TestEmit_FeatureShape(t *testing.T) {
	out := string(Emit(FeatureName, []string{"-O2", "-mavx2"}))
	for _, want := range []string{
		`"@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl"`,
		`name = "cmake_common_compile_flags"`,
		"enabled = False",
		`"-O2"`,
		`"-mavx2"`,
		"ACTION_NAMES.c_compile",
		"COMMON_COMPILE_FLAGS_FEATURES = [cmake_common_compile_flags_feature]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("emit missing %q; got:\n%s", want, out)
		}
	}

	empty := string(Emit(FeatureName, nil))
	if !strings.Contains(empty, "COMMON_COMPILE_FLAGS_FEATURES = []") {
		t.Errorf("empty emit should export an empty list; got:\n%s", empty)
	}
	if strings.Contains(empty, "feature(") {
		t.Errorf("empty emit should not render a feature; got:\n%s", empty)
	}
}
