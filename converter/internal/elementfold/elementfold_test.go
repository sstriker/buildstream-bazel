package elementfold

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/ir"
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
// TestFold_PhantomTargetScalarAttr: the round-2 stub shape's
// cc_import target may be present on only one platform (an
// arch-specific binary, a feature gated by configure). Verify
// the phantom-target fold routes StaticLibrary into
// PerPlatformScalar with a single arm, leaving the flat
// StaticLibrary empty so absent platforms don't inherit a path.
func TestFold_PhantomTargetScalarAttr(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "linuxonly", Kind: ir.KindCCImport, StaticLibrary: "lib/x86_64-linux-gnu/liblinuxonly.a"},
			},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg:      &ir.Package{Targets: nil},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(merged.Targets) != 1 {
		t.Fatalf("merged.Targets: want 1, got %d", len(merged.Targets))
	}
	got := merged.Targets[0]
	if got.StaticLibrary != "" {
		t.Errorf("phantom target flat StaticLibrary should be empty; got %q", got.StaticLibrary)
	}
	wantStatic := map[string]string{"@platforms//os:linux": "lib/x86_64-linux-gnu/liblinuxonly.a"}
	if !reflect.DeepEqual(got.PerPlatformScalar["static_library"], wantStatic) {
		t.Errorf("PerPlatformScalar[static_library] = %v; want %v",
			got.PerPlatformScalar["static_library"], wantStatic)
	}
}

// TestFold_PhantomTargetOrderSensitive: copts on a phantom
// target route through PerPlatform with a present-cell arm even
// when only one cell carries the target — phantom forces
// per-platform select() rendering so absent platforms don't see
// a flat baseline that promises flags for a target they don't
// have. Single-arm copts select() reads as
// `select({plat_with: ["-O2"], "//conditions:default": []})`.
func TestFold_PhantomTargetOrderSensitive(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "linuxonly", Kind: ir.KindCCLibrary, Srcs: []string{"a.c"}, Copts: []string{"-O2"}},
			},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg:      &ir.Package{Targets: nil},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	if len(got.Copts) != 0 {
		t.Errorf("phantom target flat Copts should be empty; got %v", got.Copts)
	}
	wantCopts := map[string][]string{"@platforms//os:linux": {"-O2"}}
	if !reflect.DeepEqual(got.PerPlatform["copts"], wantCopts) {
		t.Errorf("PerPlatform[copts] = %v; want %v", got.PerPlatform["copts"], wantCopts)
	}
}

// TestFold_PhantomTargetOnlyInLaterCell: union enumeration
// includes targets that appear only in cells[1..]. They land
// after cells[0]'s targets in declaration order. The byte-stable
// invariant for the common case (all cells declare the same set)
// is unaffected because cells[0]'s targets come first in the
// union.
func TestFold_PhantomTargetOnlyInLaterCell(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "shared", Kind: ir.KindCCLibrary, Srcs: []string{"a.c"}},
			},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "shared", Kind: ir.KindCCLibrary, Srcs: []string{"a.c"}},
				{Name: "darwin_only", Kind: ir.KindCCBinary, Srcs: []string{"darwin/main.c"}},
			},
		},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(merged.Targets) != 2 {
		t.Fatalf("merged.Targets: want 2, got %d", len(merged.Targets))
	}
	if merged.Targets[0].Name != "shared" || merged.Targets[1].Name != "darwin_only" {
		t.Errorf("union order: want [shared darwin_only]; got [%s %s]",
			merged.Targets[0].Name, merged.Targets[1].Name)
	}
}

// TestFold_PhantomTarget covers the target-presence delta:
// a target declared by some cells but not others folds into a
// phantom-target select rather than erroring. The merged
// Package includes the phantom target with PerPlatform arms
// keyed only on the cells that declared it; absent cells
// contribute no arm, and the rendered select() resolves to its
// default ([]) on those platforms — Bazel consumers depending
// on the target there see empty inputs and fail with a legible
// "no inputs" diagnostic.
func TestFold_PhantomTarget(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "libfoo", Kind: ir.KindCCLibrary, Srcs: []string{"foo.c"}},
				{Name: "linux_only", Kind: ir.KindCCBinary, Srcs: []string{"main.c"}},
			},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Targets: []ir.Target{
				{Name: "libfoo", Kind: ir.KindCCLibrary, Srcs: []string{"foo.c"}},
			},
		},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold rejected phantom-target shape: %v", err)
	}
	// Both targets land in the merged Package — union of cells'
	// target sets, in declaration order (cells[0]'s first).
	if len(merged.Targets) != 2 {
		t.Fatalf("merged.Targets: want 2 (union of cells), got %d (%v)", len(merged.Targets), merged.Targets)
	}
	if merged.Targets[0].Name != "libfoo" || merged.Targets[1].Name != "linux_only" {
		t.Errorf("merged target order: want [libfoo linux_only]; got [%s %s]",
			merged.Targets[0].Name, merged.Targets[1].Name)
	}
	// The phantom target's srcs lives in the linux arm only;
	// flat baseline is empty so the darwin (absent) cell sees []
	// at analysis time.
	phantom := merged.Targets[1]
	if len(phantom.Srcs) != 0 {
		t.Errorf("phantom target's flat Srcs should be empty (linux-only); got %v", phantom.Srcs)
	}
	wantSrcsArm := map[string][]string{"@platforms//os:linux": {"main.c"}}
	if !reflect.DeepEqual(phantom.PerPlatform["srcs"], wantSrcsArm) {
		t.Errorf("phantom target srcs arm: want %v; got %v", wantSrcsArm, phantom.PerPlatform["srcs"])
	}
	// Non-phantom target stays byte-stable: both cells agree on
	// srcs, both platforms in the matrix declare it → flat
	// baseline ["foo.c"], no PerPlatform delta. This is the
	// stability invariant the union enumeration is supposed to
	// preserve for the common case.
	libfoo := merged.Targets[0]
	if !reflect.DeepEqual(libfoo.Srcs, []string{"foo.c"}) {
		t.Errorf("non-phantom target flat Srcs: want [foo.c], got %v", libfoo.Srcs)
	}
	if len(libfoo.PerPlatform) != 0 {
		t.Errorf("non-phantom target should have no PerPlatform; got %v", libfoo.PerPlatform)
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
// pass through verbatim; the auto-detect platforms must pick
// constraint labels that are unique across the FULL matrix
// (override-platform constraints included), not just within
// the auto-detect subset, otherwise the rendered select()
// would have multiple arms matching the same Bazel platform
// at analysis time.
func TestPickSelectKeys_OperatorOverridesPartial(t *testing.T) {
	plats := []Platform{
		// Two ambiguous platforms get explicit keys.
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}, SelectKey: "//platforms:linux_x86_64"},
		{Name: "linux_aarch64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"}, SelectKey: "//platforms:linux_aarch64"},
		// darwin auto-detects: cpu:arm64 is shared with
		// linux_aarch64 (count=2 across the full matrix), so the
		// auto-detect must skip it and pick os:darwin (count=1).
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
	// darwin_arm64's cpu:arm64 collides with linux_aarch64's
	// constraint set; auto-detect must pick os:darwin (the
	// uniquely-counted constraint across the full matrix).
	if got["darwin_arm64"] != "@platforms//os:darwin" {
		t.Errorf("darwin_arm64 auto-detect = %q; want @platforms//os:darwin (cpu:arm64 collides with linux_aarch64)", got["darwin_arm64"])
	}
}

// TestPickSelectKeys_RejectsDuplicateNames: two cells sharing
// the same Platform.Name (with no SelectKey set on either)
// must error with a name-collision message rather than
// silently letting the second cell overwrite the first in the
// internal map. The auto-detect path doesn't trigger the
// final-key uniqueness check for this case — both cells would
// just collapse into one entry — so the per-iteration
// duplicate-name check is what guards it.
func TestPickSelectKeys_RejectsDuplicateNames(t *testing.T) {
	plats := []Platform{
		{Name: "linux", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
		{Name: "linux", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"}},
	}
	_, err := PickSelectKeys(plats)
	if err == nil {
		t.Fatal("expected error for duplicate Platform.Name across auto-detect cells")
	}
	if !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("error %q should mention the duplicate-name shape", err)
	}
}

// TestPickSelectKeys_RejectsDuplicateOverrideLabels: two
// platforms supplying the same select_label is an operator
// typo that would produce duplicate select() arms whose
// per-platform deltas would silently overwrite each other in
// PerPlatform. Reject up front with a message naming both
// platforms.
func TestPickSelectKeys_RejectsDuplicateOverrideLabels(t *testing.T) {
	plats := []Platform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}, SelectKey: "//platforms:shared"},
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}, SelectKey: "//platforms:shared"},
	}
	_, err := PickSelectKeys(plats)
	if err == nil {
		t.Fatal("expected error for duplicate override labels")
	}
	if !strings.Contains(err.Error(), "linux_x86_64") || !strings.Contains(err.Error(), "darwin_arm64") {
		t.Errorf("error %q should name both colliding platforms", err)
	}
}

// TestPickSelectKeys_RejectsOverrideAutoCollision: an
// override label that happens to equal another platform's
// uniquely-counted constraint produces a duplicate-arm
// collision the same way two operator-supplied labels would.
// Final-key validation catches it.
func TestPickSelectKeys_RejectsOverrideAutoCollision(t *testing.T) {
	plats := []Platform{
		// linux_x86_64 will auto-detect to @platforms//cpu:x86_64
		// (uniquely-counted across the matrix).
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
		// darwin_arm64's operator override pathologically chose
		// the exact label linux_x86_64 auto-detected.
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}, SelectKey: "@platforms//cpu:x86_64"},
	}
	_, err := PickSelectKeys(plats)
	if err == nil {
		t.Fatal("expected error for override-vs-auto collision")
	}
	if !strings.Contains(err.Error(), "linux_x86_64") || !strings.Contains(err.Error(), "darwin_arm64") {
		t.Errorf("error %q should name both colliding platforms", err)
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

// TestFold_CCImportStaticLibraryDivergesAcrossPlatforms: the
// round-2 fallback's main divergence axis. cells produce
// cc_import stubs whose static_library paths reflect their
// platform's install_tree.tar layout (multiarch lib dir on
// linux, flat lib/ on darwin). The fold should land each cell's
// path in PerPlatformScalar["static_library"][SelectKey] with
// the flat StaticLibrary cleared, so emit renders a select().
func TestFold_CCImportStaticLibraryDivergesAcrossPlatforms(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Name: "libfoo",
			Targets: []ir.Target{{
				Name:          "foo",
				Kind:          ir.KindCCImport,
				StaticLibrary: "lib/x86_64-linux-gnu/libfoo.a",
			}},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Name: "libfoo",
			Targets: []ir.Target{{
				Name:          "foo",
				Kind:          ir.KindCCImport,
				StaticLibrary: "lib/libfoo.a",
			}},
		},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(merged.Targets) != 1 {
		t.Fatalf("merged: want 1 target, got %d", len(merged.Targets))
	}
	got := merged.Targets[0]
	if got.StaticLibrary != "" {
		t.Errorf("flat StaticLibrary should be empty when cells disagree; got %q", got.StaticLibrary)
	}
	wantDeltas := map[string]string{
		"@platforms//os:linux":  "lib/x86_64-linux-gnu/libfoo.a",
		"@platforms//os:darwin": "lib/libfoo.a",
	}
	if !reflect.DeepEqual(got.PerPlatformScalar["static_library"], wantDeltas) {
		t.Errorf("PerPlatformScalar[static_library] = %v; want %v", got.PerPlatformScalar["static_library"], wantDeltas)
	}
}

// TestFold_CCImportStaticLibraryAgreesAcrossPlatforms: when every
// cell happens to produce the same path (e.g. a single-arch
// fixture, or a header-only library whose .a lives at the same
// install path on every platform), the flat StaticLibrary holds
// the value and PerPlatformScalar stays empty. This is the
// byte-stability contract for round-2 stubs that don't diverge.
func TestFold_CCImportStaticLibraryAgreesAcrossPlatforms(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Name: "libfoo",
			Targets: []ir.Target{{
				Name:          "foo",
				Kind:          ir.KindCCImport,
				StaticLibrary: "lib/libfoo.a",
			}},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Name: "libfoo",
			Targets: []ir.Target{{
				Name:          "foo",
				Kind:          ir.KindCCImport,
				StaticLibrary: "lib/libfoo.a",
			}},
		},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	if got.StaticLibrary != "lib/libfoo.a" {
		t.Errorf("flat StaticLibrary = %q; want lib/libfoo.a", got.StaticLibrary)
	}
	if len(got.PerPlatformScalar) != 0 {
		t.Errorf("PerPlatformScalar should be empty when all cells agree; got %v", got.PerPlatformScalar)
	}
}

// TestFold_CCImportPartialPlatformShape: cc_import where one
// platform has only static_library and another has only
// shared_library — the flat baseline clears for both attrs, and
// each delta map only records the platform(s) that populated it.
// Empty values are omitted from the delta map so emit doesn't
// render `select({plat: ""})` (which would be a Bazel attribute
// error).
func TestFold_CCImportPartialPlatformShape(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Name: "libfoo",
			Targets: []ir.Target{{
				Name:          "foo",
				Kind:          ir.KindCCImport,
				StaticLibrary: "lib/libfoo.a",
			}},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Name: "libfoo",
			Targets: []ir.Target{{
				Name:          "foo",
				Kind:          ir.KindCCImport,
				SharedLibrary: "lib/libfoo.dylib",
			}},
		},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	if got.StaticLibrary != "" || got.SharedLibrary != "" {
		t.Errorf("flat path attrs should clear when cells disagree; got static=%q shared=%q", got.StaticLibrary, got.SharedLibrary)
	}
	wantStatic := map[string]string{"@platforms//os:linux": "lib/libfoo.a"}
	if !reflect.DeepEqual(got.PerPlatformScalar["static_library"], wantStatic) {
		t.Errorf("PerPlatformScalar[static_library] = %v; want %v", got.PerPlatformScalar["static_library"], wantStatic)
	}
	wantShared := map[string]string{"@platforms//os:darwin": "lib/libfoo.dylib"}
	if !reflect.DeepEqual(got.PerPlatformScalar["shared_library"], wantShared) {
		t.Errorf("PerPlatformScalar[shared_library] = %v; want %v", got.PerPlatformScalar["shared_library"], wantShared)
	}
}

// TestFold_ShBinarySrcsDivergeAcrossPlatforms: sh_binary stubs
// for EXECUTABLE targets reference the installed binary path
// inside install_tree.tar. Arch-tagged binary names (e.g.
// `bin/foo-x86_64` vs `bin/foo`) produce divergent srcs across
// cells; the fold lifts each cell's srcs entry into PerPlatform
// with no flat baseline (the items are disjoint), and emit
// renders srcs as a bare select().
func TestFold_ShBinarySrcsDivergeAcrossPlatforms(t *testing.T) {
	linux := Cell{
		Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"},
		Pkg: &ir.Package{
			Name: "tool",
			Targets: []ir.Target{{
				Name: "tool",
				Kind: ir.KindShBinary,
				Srcs: []string{"bin/tool-linux"},
			}},
		},
	}
	darwin := Cell{
		Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"},
		Pkg: &ir.Package{
			Name: "tool",
			Targets: []ir.Target{{
				Name: "tool",
				Kind: ir.KindShBinary,
				Srcs: []string{"bin/tool-darwin"},
			}},
		},
	}
	merged, err := Fold([]Cell{linux, darwin})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := merged.Targets[0]
	if len(got.Srcs) != 0 {
		t.Errorf("flat Srcs should be empty when items disjoint; got %v", got.Srcs)
	}
	wantSrcs := map[string][]string{
		"@platforms//os:linux":  {"bin/tool-linux"},
		"@platforms//os:darwin": {"bin/tool-darwin"},
	}
	if !reflect.DeepEqual(got.PerPlatform["srcs"], wantSrcs) {
		t.Errorf("PerPlatform[srcs] = %v; want %v", got.PerPlatform["srcs"], wantSrcs)
	}
}
