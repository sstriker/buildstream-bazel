package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// grid2D builds byCell for one target "foo" over Debug/Release ×
// on/off, with per-cell defines.
func grid2D(defsByCell map[string][]string) map[string]map[string]fileapi.Target {
	mk := func(defs []string) fileapi.Target {
		cds := []fileapi.CompileDefine{{Define: "COMMON=1"}}
		for _, d := range defs {
			cds = append(cds, fileapi.CompileDefine{Define: d})
		}
		return fileapi.Target{Name: "foo", CompileGroups: []fileapi.CompileGroup{{Language: "C", Defines: cds}}}
	}
	out := map[string]fileapi.Target{}
	for _, c := range []string{"Debug", "Release"} {
		for _, v := range []string{"//options:foo_feature_on", "//options:foo_feature_off"} {
			out[Cell2DKey(c, v)] = mk(defsByCell[Cell2DKey(c, v)])
		}
	}
	return map[string]map[string]fileapi.Target{"foo": out}
}

var (
	cfgs2D = []string{"Debug", "Release"}
	arms2D = []string{"//options:foo_feature_on", "//options:foo_feature_off"} // configured value first
)

func TestApplyOptionFold2D_PureOption(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}}}
	byCell := grid2D(map[string][]string{
		Cell2DKey("Debug", arms2D[0]):   {"FEAT=1"},
		Cell2DKey("Release", arms2D[0]): {"FEAT=1"},
	})
	lifted, groups := ApplyOptionFold2D(pkg, byCell, cfgs2D, arms2D, "", "", nil, "//options:foo_feature")
	if !reflect.DeepEqual(lifted, []string{"foo"}) || len(groups) != 0 {
		t.Fatalf("lifted=%v groups=%v", lifted, groups)
	}
	defines := pkg.Targets[0].PerPlatform["defines"]
	if got := defines["//options:foo_feature_on"]; len(got) != 1 || got[0] != "FEAT=1" {
		t.Errorf("option arm: %v", got)
	}
	if _, ok := defines["//options:debug_and_foo_feature_on"]; ok {
		t.Errorf("pure option fact must not produce AND arms: %v", defines)
	}
}

func TestApplyOptionFold2D_PureConfigSkipped(t *testing.T) {
	// A fact present in Debug under BOTH option values is the base
	// multi-config fold's job — the 2D fold must not re-emit it.
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "foo", Kind: ir.KindCCLibrary,
		// The base fold already emitted this arm.
		PerPlatform: map[string]map[string][]string{
			"defines": {"//config:debug": {"DBG=1"}},
		},
	}}}
	byCell := grid2D(map[string][]string{
		Cell2DKey("Debug", arms2D[0]): {"DBG=1"},
		Cell2DKey("Debug", arms2D[1]): {"DBG=1"},
	})
	lifted, groups := ApplyOptionFold2D(pkg, byCell, cfgs2D, arms2D, "", "", nil, "//options:foo_feature")
	if len(lifted) != 0 || len(groups) != 0 {
		t.Fatalf("pure-config fact must not lift: lifted=%v groups=%v", lifted, groups)
	}
	if got := pkg.Targets[0].PerPlatform["defines"]["//config:debug"]; len(got) != 1 || got[0] != "DBG=1" {
		t.Errorf("base fold's config arm must stay intact: %v", got)
	}
}

func TestApplyOptionFold2D_MixedFact(t *testing.T) {
	// Present ONLY under (Debug, on): the base fold — measuring at
	// the configured value (on) — emitted a plain //config:debug arm
	// that over-applies under (Debug, off). The 2D fold moves the
	// fact onto the AND arm and strips the plain config arm.
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "foo", Kind: ir.KindCCLibrary,
		PerPlatform: map[string]map[string][]string{
			"defines": {"//config:debug": {"FOO_DEBUG=1", "KEEP=1"}},
		},
	}}}
	byCell := grid2D(map[string][]string{
		Cell2DKey("Debug", arms2D[0]): {"FOO_DEBUG=1"},
	})
	lifted, groups := ApplyOptionFold2D(pkg, byCell, cfgs2D, arms2D, "", "", nil, "//options:foo_feature")
	if !reflect.DeepEqual(lifted, []string{"foo"}) {
		t.Fatalf("lifted = %v", lifted)
	}
	if len(groups) != 1 || groups[0].Name != "debug_and_foo_feature_on" ||
		!reflect.DeepEqual(groups[0].MatchAll, []string{"//config:debug", "//options:foo_feature_on"}) {
		t.Fatalf("groups = %+v", groups)
	}
	defines := pkg.Targets[0].PerPlatform["defines"]
	if got := defines["//options:debug_and_foo_feature_on"]; len(got) != 1 || got[0] != "FOO_DEBUG=1" {
		t.Errorf("AND arm: %v", got)
	}
	if got := defines["//config:debug"]; len(got) != 1 || got[0] != "KEEP=1" {
		t.Errorf("plain config arm must lose the mixed fact and keep the rest: %v", got)
	}
	if fam := pkg.SelectArmFamilies["//options:debug_and_foo_feature_on"]; fam != "//config:build_type+//options:foo_feature" {
		t.Errorf("group family: %q", fam)
	}
}

// TestApplyOptionFold2D_PureReplacementStillLifts pins the lift
// signal on arm MUTATION rather than arm-count delta: a mixed fact
// that exactly replaces a base-fold //config arm with an AND arm
// leaves the count unchanged, but the package now references
// //options labels — skipping the flag emit would break the BUILD.
func TestApplyOptionFold2D_PureReplacementStillLifts(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "foo", Kind: ir.KindCCLibrary,
		PerPlatform: map[string]map[string][]string{
			"defines": {"//config:debug": {"FOO_DEBUG=1"}},
		},
	}}}
	byCell := grid2D(map[string][]string{
		Cell2DKey("Debug", arms2D[0]): {"FOO_DEBUG=1"},
	})
	lifted, groups := ApplyOptionFold2D(pkg, byCell, cfgs2D, arms2D, "", "", nil, "//options:foo_feature")
	if !reflect.DeepEqual(lifted, []string{"foo"}) {
		t.Fatalf("replacement must still lift: lifted=%v", lifted)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	defines := pkg.Targets[0].PerPlatform["defines"]
	if _, ok := defines["//config:debug"]; ok {
		t.Errorf("emptied config arm should prune: %v", defines)
	}
	if got := defines["//options:debug_and_foo_feature_on"]; len(got) != 1 {
		t.Errorf("AND arm: %v", got)
	}
}

// TestApplyOptionFold2D_LocalDefineScopeMirrored pins scope
// preservation: a define the base fold routed to local_defines keeps
// that (non-transitive) spelling on the 2D fold's AND arms — landing
// it under `defines` would widen it to transitive for consumers.
func TestApplyOptionFold2D_LocalDefineScopeMirrored(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "foo", Kind: ir.KindCCLibrary,
		PerPlatform: map[string]map[string][]string{
			"local_defines": {"//config:debug": {"FOO_DEBUG=1"}},
		},
	}}}
	byCell := grid2D(map[string][]string{
		Cell2DKey("Debug", arms2D[0]): {"FOO_DEBUG=1"},
	})
	lifted, _ := ApplyOptionFold2D(pkg, byCell, cfgs2D, arms2D, "", "", nil, "//options:foo_feature")
	if !reflect.DeepEqual(lifted, []string{"foo"}) {
		t.Fatalf("lifted = %v", lifted)
	}
	pp := pkg.Targets[0].PerPlatform
	if got := pp["local_defines"]["//options:debug_and_foo_feature_on"]; len(got) != 1 || got[0] != "FOO_DEBUG=1" {
		t.Errorf("AND arm must keep the local_defines scope: %v", pp)
	}
	if arms, ok := pp["defines"]; ok {
		if _, bad := arms["//options:debug_and_foo_feature_on"]; bad {
			t.Errorf("token widened to transitive defines: %v", arms)
		}
	}
	if _, ok := pp["local_defines"]["//config:debug"]; ok {
		t.Errorf("emptied local_defines config arm should prune: %v", pp)
	}
}
