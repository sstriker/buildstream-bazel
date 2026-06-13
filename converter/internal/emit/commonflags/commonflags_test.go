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

// TestHoistCommonFlagsToConstants_AllAxes: the self-contained mode hoists
// copts, local_defines (a sorted set), AND linkopts independently — each gets
// its own shared-prefix strip + Prepend* mark, sharing the one load label. This
// is the BDE shape: a project-wide flag set repeated on every target.
func TestHoistCommonFlagsToConstants_AllAxes(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{
			Name: "a", Kind: ir.KindCCLibrary,
			Copts:        []string{"-O3", "-pthread"},
			LocalDefines: []string{"NDEBUG", "BDE_BUILD_TARGET_OPT"}, // unsorted on input
			LinkOpts:     []string{"-O3", "-lrt"},
		},
		{
			Name: "b", Kind: ir.KindCCBinary,
			Copts: []string{"-O3", "-pthread", "-Wextra"},
			// ZEXTRA sorts AFTER the shared defines, so the common sorted prefix
			// still captures both (a define that sorted BEFORE them would cap the
			// prefix — the conservative set-vs-prefix limitation, covered below).
			LocalDefines: []string{"BDE_BUILD_TARGET_OPT", "NDEBUG", "ZEXTRA"},
			LinkOpts:     []string{"-O3", "-lrt", "-lm"},
		},
	}}
	copts, ld, lo := HoistCommonFlagsToConstants(pkg, "//pkg:common_compile_flags.bzl")
	if !eq(copts, []string{"-O3", "-pthread"}) {
		t.Errorf("copts prefix = %v; want [-O3 -pthread]", copts)
	}
	// local_defines is a set: sorted, then the common LEADING sorted elements.
	if !eq(ld, []string{"BDE_BUILD_TARGET_OPT", "NDEBUG"}) {
		t.Errorf("local_defines prefix = %v; want [BDE_BUILD_TARGET_OPT NDEBUG]", ld)
	}
	if !eq(lo, []string{"-O3", "-lrt"}) {
		t.Errorf("linkopts prefix = %v; want [-O3 -lrt]", lo)
	}
	if pkg.CommonCoptsLabel != "//pkg:common_compile_flags.bzl" {
		t.Errorf("CommonCoptsLabel = %q; want the load label", pkg.CommonCoptsLabel)
	}
	// a: every axis fully consumed by the prefix → nil delta + all three marks.
	a := pkg.Targets[0]
	if a.Copts != nil || a.LocalDefines != nil || a.LinkOpts != nil {
		t.Errorf("a deltas: copts=%v ld=%v lo=%v; want all nil", a.Copts, a.LocalDefines, a.LinkOpts)
	}
	if !a.PrependCommonCopts || !a.PrependCommonLocalDefines || !a.PrependCommonLinkopts {
		t.Errorf("a marks: copts=%v ld=%v lo=%v; want all true", a.PrependCommonCopts, a.PrependCommonLocalDefines, a.PrependCommonLinkopts)
	}
	// b: keeps its per-target delta on each axis (local_defines stays sorted).
	b := pkg.Targets[1]
	if !eq(b.Copts, []string{"-Wextra"}) || !eq(b.LocalDefines, []string{"ZEXTRA"}) || !eq(b.LinkOpts, []string{"-lm"}) {
		t.Errorf("b deltas: copts=%v ld=%v lo=%v; want [-Wextra] [ZEXTRA] [-lm]", b.Copts, b.LocalDefines, b.LinkOpts)
	}
}

// TestHoistCommonFlagsToConstants_SetConservatism: local_defines is hoisted as
// the common LEADING sorted prefix, not the maximal common subset. A define
// that sorts BEFORE a shared one in just one target caps the prefix there — so
// the shared define after it is NOT hoisted (conservative, never wrong).
func TestHoistCommonFlagsToConstants_SetConservatism(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindCCLibrary, LocalDefines: []string{"BB", "NDEBUG"}},
		{Name: "b", Kind: ir.KindCCBinary, LocalDefines: []string{"AA", "BB", "NDEBUG"}},
	}}
	_, ld, _ := HoistCommonFlagsToConstants(pkg, "//pkg:common_compile_flags.bzl")
	// Sorted: a=[BB,NDEBUG], b=[AA,BB,NDEBUG]. They diverge at index 0 (BB vs
	// AA), so NOTHING hoists even though {BB,NDEBUG} is common.
	if ld != nil {
		t.Errorf("local_defines prefix = %v; want nil (set-vs-prefix conservatism)", ld)
	}
}

// TestEmitConstants_MultiAxis: COMMON_COPTS always renders; COMMON_LOCAL_DEFINES
// / COMMON_LINKOPTS render only when non-empty, so a copts-only project's .bzl
// stays byte-identical to the copts-only era.
func TestEmitConstants_MultiAxis(t *testing.T) {
	all := string(EmitConstants([]string{"-O3"}, []string{"NDEBUG"}, []string{"-lrt"}))
	for _, want := range []string{"COMMON_COPTS = [", "COMMON_LOCAL_DEFINES = [", "COMMON_LINKOPTS = [", `"NDEBUG"`, `"-lrt"`} {
		if !strings.Contains(all, want) {
			t.Errorf("EmitConstants missing %q; got:\n%s", want, all)
		}
	}
	coptsOnly := string(EmitConstants([]string{"-O3"}, nil, nil))
	// Assert on the assignment form — the header comment documents all three
	// constant names, so a bare-name Contains would false-match.
	if strings.Contains(coptsOnly, "COMMON_LOCAL_DEFINES = ") || strings.Contains(coptsOnly, "COMMON_LINKOPTS = ") {
		t.Errorf("copts-only EmitConstants should omit the other constant assignments; got:\n%s", coptsOnly)
	}
	if coptsOnly != string(EmitConstant([]string{"-O3"})) {
		t.Errorf("copts-only EmitConstants must match the EmitConstant shim byte-for-byte")
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
