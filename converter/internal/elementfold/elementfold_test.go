package elementfold

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
)

// TestFold_SingleCellIdentity: the N=1 degenerate case must
// produce a Package whose targets carry the original cell's
// data and an empty PerPlatform map. This is the contract that
// "single-platform conversion is the N=1 case of the unified
// flow" depends on — emit's select() rendering treats nil/empty
// PerPlatform identically, so the rendered BUILD.bazel matches
// today's single-platform shape exactly.
func TestFold_SingleCellIdentity(t *testing.T) {
	pkg := &ir.Package{
		Name: "hello",
		Targets: []ir.Target{{
			Name:    "libfoo",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"foo.c"},
			Hdrs:    []string{"foo.h"},
			Copts:   []string{"-Wall"},
			Defines: []string{"X=1"},
		}},
	}
	plat := Platform{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"}
	merged, err := Fold([]Cell{{Platform: plat, Pkg: pkg}})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if merged.Name != pkg.Name {
		t.Errorf("merged.Name = %q; want %q", merged.Name, pkg.Name)
	}
	if len(merged.Targets) != 1 {
		t.Fatalf("expected 1 target; got %d", len(merged.Targets))
	}
	got := merged.Targets[0]
	if !reflect.DeepEqual(got.Srcs, []string{"foo.c"}) {
		t.Errorf("Srcs = %v; want [foo.c]", got.Srcs)
	}
	if !reflect.DeepEqual(got.Hdrs, []string{"foo.h"}) {
		t.Errorf("Hdrs = %v; want [foo.h]", got.Hdrs)
	}
	if got.PerPlatform != nil {
		t.Errorf("PerPlatform should be nil for single-cell; got %v", got.PerPlatform)
	}
}

// TestFold_SrcsDivergeAcrossPlatforms: items unique to one
// platform land in PerPlatform under that platform's
// SelectKey; items shared across all platforms stay in the
// flat baseline.
func TestFold_SrcsDivergeAcrossPlatforms(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Name: "hello",
			Targets: []ir.Target{{
				Name: "libfoo",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"common.c", "linux/foo.c"},
			}},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Name: "hello",
			Targets: []ir.Target{{
				Name: "libfoo",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"common.c", "darwin/foo.c"},
			}},
		},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	if !reflect.DeepEqual(got.Srcs, []string{"common.c"}) {
		t.Errorf("baseline Srcs = %v; want [common.c]", got.Srcs)
	}
	wantPerPlatform := map[string]map[string][]string{
		"srcs": {
			"@platforms//os:linux":  {"linux/foo.c"},
			"@platforms//os:darwin": {"darwin/foo.c"},
		},
	}
	if !reflect.DeepEqual(got.PerPlatform, wantPerPlatform) {
		t.Errorf("PerPlatform = %v; want %v", got.PerPlatform, wantPerPlatform)
	}
}

// TestFold_MissingTargetRejected: a target present in some
// cells but missing from others is an error. select() can't
// conditionally instantiate a target at the package level.
func TestFold_MissingTargetRejected(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "libfoo", Kind: ir.KindCCLibrary},
				{Name: "linux_only", Kind: ir.KindCCBinary},
			},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "libfoo", Kind: ir.KindCCLibrary},
			},
		},
	}
	_, err := Fold([]Cell{linux, darwin})
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !strings.Contains(err.Error(), "linux_only") && !strings.Contains(err.Error(), "different") && !strings.Contains(err.Error(), "missing") && !strings.Contains(err.Error(), "targets but cell") {
		t.Errorf("error %q should reference the missing-target shape", err)
	}
}

// TestFold_BooleanDisagreementRejected: Linkstatic differing
// across cells is a fundamental shape divergence the fold
// can't paper over with select() (Bazel attribute select() on
// a bool would need a config_setting + a different rule
// instance). Reject it.
func TestFold_BooleanDisagreementRejected(t *testing.T) {
	a := Cell{
		Platform: Platform{Name: "a", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary, Linkstatic: true,
		}}},
	}
	b := Cell{
		Platform: Platform{Name: "b", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary, Linkstatic: false,
		}}},
	}
	_, err := Fold([]Cell{a, b})
	if err == nil {
		t.Fatal("expected error for Linkstatic disagreement")
	}
	if !strings.Contains(err.Error(), "Linkstatic") {
		t.Errorf("error %q should name Linkstatic", err)
	}
}

// TestFold_CoptsIdenticalSequenceFoldsToBaseline: when every
// cell carries the same copts sequence, the merged target gets
// that sequence as a flat baseline and no PerPlatform deltas.
// This is the "no divergence" path; emit produces today's
// pre-PerPlatform shape.
func TestFold_CoptsIdenticalSequenceFoldsToBaseline(t *testing.T) {
	mk := func(name string) Cell {
		return Cell{
			Platform: Platform{
				Name:        name,
				Constraints: []string{"@platforms//os:" + name},
				SelectKey:   "@platforms//os:" + name,
			},
			Pkg: &ir.Package{Targets: []ir.Target{{
				Name: "lib", Kind: ir.KindCCLibrary,
				Copts: []string{"-Wall", "-O2"},
			}}},
		}
	}
	merged, err := Fold([]Cell{mk("linux"), mk("darwin")})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	wantBaseline := []string{"-Wall", "-O2"}
	if !reflect.DeepEqual(got.Copts, wantBaseline) {
		t.Errorf("baseline copts = %v; want %v", got.Copts, wantBaseline)
	}
	if _, has := got.PerPlatform["copts"]; has {
		t.Errorf("expected no copts deltas when all cells agree; got %v",
			got.PerPlatform["copts"])
	}
}

// TestFold_CoptsDivergentSequenceRoutesFullLists: when copts
// sequences differ across cells (extra items, reorder, or
// both), the merged target's flat baseline is empty and each
// cell's full copts sequence routes verbatim through its
// SelectKey arm. Set-membership partitioning would be unsafe
// for order-sensitive flags: it could re-sequence one
// platform's flags into another's order and silently flip
// compiler semantics (last-flag-wins, include precedence,
// etc.).
func TestFold_CoptsDivergentSequenceRoutesFullLists(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			Copts: []string{"-Wall", "-O2"},
		}}},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			Copts: []string{"-Wall", "-O2", "-mmacosx-version-min=11.0"},
		}}},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	if len(got.Copts) != 0 {
		t.Errorf("expected empty baseline copts when sequences diverge; got %v", got.Copts)
	}
	if !reflect.DeepEqual(got.PerPlatform["copts"]["@platforms//os:linux"],
		[]string{"-Wall", "-O2"}) {
		t.Errorf("linux copts arm = %v; want [-Wall -O2]",
			got.PerPlatform["copts"]["@platforms//os:linux"])
	}
	if !reflect.DeepEqual(got.PerPlatform["copts"]["@platforms//os:darwin"],
		[]string{"-Wall", "-O2", "-mmacosx-version-min=11.0"}) {
		t.Errorf("darwin copts arm = %v; want full darwin sequence",
			got.PerPlatform["copts"]["@platforms//os:darwin"])
	}
}

// TestFold_CoptsReorderRoutesFullLists: same flag set but in
// different order across cells must NOT collapse to a baseline
// — that would force one cell's order onto another and could
// silently change last-flag-wins semantics. Both cells get the
// full list under their SelectKey.
func TestFold_CoptsReorderRoutesFullLists(t *testing.T) {
	a := Cell{
		Platform: Platform{Name: "a", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			Copts: []string{"-O0", "-O2"}, // -O2 wins
		}}},
	}
	b := Cell{
		Platform: Platform{Name: "b", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			Copts: []string{"-O2", "-O0"}, // -O0 wins
		}}},
	}
	merged, err := Fold([]Cell{a, b})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	if len(got.Copts) != 0 {
		t.Errorf("expected empty baseline copts on reorder; got %v", got.Copts)
	}
	if !reflect.DeepEqual(got.PerPlatform["copts"]["@platforms//os:linux"],
		[]string{"-O0", "-O2"}) {
		t.Errorf("linux copts arm = %v; want [-O0 -O2]",
			got.PerPlatform["copts"]["@platforms//os:linux"])
	}
	if !reflect.DeepEqual(got.PerPlatform["copts"]["@platforms//os:darwin"],
		[]string{"-O2", "-O0"}) {
		t.Errorf("darwin copts arm = %v; want [-O2 -O0]",
			got.PerPlatform["copts"]["@platforms//os:darwin"])
	}
}

// TestPickSelectKeys_SingleAxisVariesByCPU: classic
// {linux_x86_64, linux_aarch64} matrix — os shared, cpu
// distinguishes. Each platform's SelectKey is its cpu label.
func TestPickSelectKeys_SingleAxisVariesByCPU(t *testing.T) {
	plats := []Platform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
		{Name: "linux_aarch64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"}},
	}
	keys, err := PickSelectKeys(plats)
	if err != nil {
		t.Fatalf("PickSelectKeys: %v", err)
	}
	want := map[string]string{
		"linux_x86_64":  "@platforms//cpu:x86_64",
		"linux_aarch64": "@platforms//cpu:arm64",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v; want %v", keys, want)
	}
}

// TestPickSelectKeys_BothAxesVaryEachUnique: {linux_x86_64,
// darwin_arm64} — both axes vary, but each platform has both a
// unique os AND a unique cpu. Either axis works for select();
// the algorithm picks the lexicographically-smaller label per
// platform. @platforms//cpu:* sorts before @platforms//os:*.
func TestPickSelectKeys_BothAxesVaryEachUnique(t *testing.T) {
	plats := []Platform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}},
	}
	keys, err := PickSelectKeys(plats)
	if err != nil {
		t.Fatalf("PickSelectKeys: %v", err)
	}
	if keys["linux_x86_64"] != "@platforms//cpu:x86_64" {
		t.Errorf("linux_x86_64 SelectKey = %q; want @platforms//cpu:x86_64", keys["linux_x86_64"])
	}
	if keys["darwin_arm64"] != "@platforms//cpu:arm64" {
		t.Errorf("darwin_arm64 SelectKey = %q; want @platforms//cpu:arm64", keys["darwin_arm64"])
	}
}

// TestPickSelectKeys_AmbiguousMatrix: {linux_x86_64,
// linux_aarch64, darwin_arm64} — linux_aarch64 has no
// constraint that uniquely identifies it (cpu:arm64 is shared
// with darwin_arm64; os:linux is shared with linux_x86_64).
// Single-axis select() can't express this; the operator must
// declare a config_setting per platform and pass it in
// explicitly. Error message names the offending platform.
func TestPickSelectKeys_AmbiguousMatrix(t *testing.T) {
	plats := []Platform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
		{Name: "linux_aarch64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"}},
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}},
	}
	_, err := PickSelectKeys(plats)
	if err == nil {
		t.Fatal("expected error for ambiguous matrix")
	}
	if !strings.Contains(err.Error(), "linux_aarch64") {
		t.Errorf("error %q should name the offending platform", err)
	}
	if !strings.Contains(err.Error(), "config_setting") {
		t.Errorf("error %q should mention config_setting escalation", err)
	}
}

// TestPickSelectKeys_OperatorOverridesAll: same ambiguous
// matrix the auto-detect path rejects, but every platform
// supplies a SelectKey explicitly (the escalation contract:
// operator declares a config_setting per platform in their
// //platforms package). PickSelectKeys passes the supplied
// keys through verbatim with no auto-detection.
func TestPickSelectKeys_OperatorOverridesAll(t *testing.T) {
	plats := []Platform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}, SelectKey: "//platforms:linux_x86_64"},
		{Name: "linux_aarch64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"}, SelectKey: "//platforms:linux_aarch64"},
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}, SelectKey: "//platforms:darwin_arm64"},
	}
	got, err := PickSelectKeys(plats)
	if err != nil {
		t.Fatalf("PickSelectKeys: %v", err)
	}
	want := map[string]string{
		"linux_x86_64":  "//platforms:linux_x86_64",
		"linux_aarch64": "//platforms:linux_aarch64",
		"darwin_arm64":  "//platforms:darwin_arm64",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("platform %q: SelectKey = %q; want %q", name, got[name], w)
		}
	}
}

// TestPickSelectKeys_OperatorOverridesPartial: a mixed matrix
// — some platforms have a SelectKey override, the rest
// auto-detect off their constraints. The override platforms
// pass through verbatim; their constraint labels are excluded
// from the auto-detect count so they don't taint the
// uniqueness check for the auto-detect platforms.
func TestPickSelectKeys_OperatorOverridesPartial(t *testing.T) {
	plats := []Platform{
		// Two ambiguous platforms get explicit keys.
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}, SelectKey: "//platforms:linux_x86_64"},
		{Name: "linux_aarch64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"}, SelectKey: "//platforms:linux_aarch64"},
		// darwin auto-detects: its os:darwin is unique once the
		// linux entries are out of the count.
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}},
	}
	got, err := PickSelectKeys(plats)
	if err != nil {
		t.Fatalf("PickSelectKeys: %v", err)
	}
	if got["linux_x86_64"] != "//platforms:linux_x86_64" {
		t.Errorf("linux_x86_64 override = %q", got["linux_x86_64"])
	}
	if got["linux_aarch64"] != "//platforms:linux_aarch64" {
		t.Errorf("linux_aarch64 override = %q", got["linux_aarch64"])
	}
	// darwin_arm64 has both os:darwin and cpu:arm64 unique once
	// the two override platforms are excluded from the count;
	// PickSelectKeys' lex-min tiebreaker picks cpu:arm64.
	if got["darwin_arm64"] != "@platforms//cpu:arm64" {
		t.Errorf("darwin_arm64 auto-detect = %q; want @platforms//cpu:arm64", got["darwin_arm64"])
	}
}

// TestFold_PackageNameMismatch: the per-element fold composes
// per-platform IRs of the SAME element. If their Package.Name
// disagrees, that's a programmer error elsewhere in the flow,
// not legitimately divergent data.
func TestFold_PackageNameMismatch(t *testing.T) {
	a := Cell{
		Platform: Platform{Name: "a", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg:      &ir.Package{Name: "hello"},
	}
	b := Cell{
		Platform: Platform{Name: "b", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg:      &ir.Package{Name: "world"},
	}
	_, err := Fold([]Cell{a, b})
	if err == nil {
		t.Fatal("expected error for mismatched Package.Name")
	}
	if !strings.Contains(err.Error(), "Package.Name") {
		t.Errorf("error %q should name Package.Name", err)
	}
}
