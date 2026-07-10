package main

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

func TestCmakeTruthy(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"ON", true}, {"on", true}, {"1", true}, {"TRUE", true}, {"YES", true},
		{"OFF", false}, {"0", false}, {"FALSE", false}, {"NO", false}, {"N", false},
		{"", false}, {"IGNORE", false}, {"NOTFOUND", false}, {"ZLIB-NOTFOUND", false},
		{"y", true}, {"anything", true},
	} {
		if got := cmakeTruthy(tc.in); got != tc.want {
			t.Errorf("cmakeTruthy(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalizeFlipTarget_RewritesScratchPaths(t *testing.T) {
	tgt := fileapi.Target{
		Name:    "foo",
		Sources: []fileapi.TargetSource{{Path: "/b/scratch-opt-FOO/gen.c"}},
		CompileGroups: []fileapi.CompileGroup{{
			Language:                "C",
			Includes:                []fileapi.CompileInclude{{Path: "/b/scratch-opt-FOO/include"}},
			Defines:                 []fileapi.CompileDefine{{Define: "DIR=/b/scratch-opt-FOO"}},
			CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-I/b/scratch-opt-FOO/x"}},
		}},
		Link: &fileapi.TargetLink{
			CommandFragments: []fileapi.CommandFragment{{Fragment: "/b/scratch-opt-FOO/libz.a", Role: "libraries"}},
		},
	}
	canonicalizeFlipTarget(&tgt, "/b/scratch-opt-FOO", "/b/host")
	if got := tgt.Sources[0].Path; got != "/b/host/gen.c" {
		t.Errorf("source path: %q", got)
	}
	if got := tgt.CompileGroups[0].Includes[0].Path; got != "/b/host/include" {
		t.Errorf("include path: %q", got)
	}
	if got := tgt.CompileGroups[0].Defines[0].Define; got != "DIR=/b/host" {
		t.Errorf("define: %q", got)
	}
	if got := tgt.CompileGroups[0].CompileCommandFragments[0].Fragment; got != "-I/b/host/x" {
		t.Errorf("compile fragment: %q", got)
	}
	if got := tgt.Link.CommandFragments[0].Fragment; got != "/b/host/libz.a" {
		t.Errorf("link fragment: %q", got)
	}
}

func TestResolveOptionSpecs(t *testing.T) {
	cache := fileapi.Cache{Entries: []fileapi.CacheEntry{
		{Name: "FEAT", Type: "BOOL", Value: "ON"},
		{Name: "BACKEND", Type: "STRING", Value: "ref",
			Properties: []fileapi.CacheEntryProp{{Name: "STRINGS", Value: "ref;fast;turbo"}}},
		{Name: "FREEFORM", Type: "STRING", Value: "hello"},
		{Name: "PATHY", Type: "FILEPATH", Value: "/x"},
		{Name: "BAD_ENUM", Type: "STRING", Value: "zzz",
			Properties: []fileapi.CacheEntryProp{{Name: "STRINGS", Value: "a;b"}}},
	}}
	specs, flips := resolveOptionSpecs([]string{"FEAT", "BACKEND", "FREEFORM", "PATHY", "BAD_ENUM", "MISSING"}, cache)
	if len(specs) != 2 {
		t.Fatalf("specs: got %d (%+v), want 2 (FEAT + BACKEND)", len(specs), specs)
	}
	if specs[0].name != "FEAT" || specs[0].enum || !specs[0].baseOn || specs[0].baseLabel != "//options:feat_on" {
		t.Errorf("FEAT spec: %+v", specs[0])
	}
	if specs[1].name != "BACKEND" || !specs[1].enum || specs[1].baseValue != "ref" || len(specs[1].values) != 3 {
		t.Errorf("BACKEND spec: %+v", specs[1])
	}
	// FEAT gets one flip (OFF); BACKEND two (fast, turbo).
	if len(flips) != 3 {
		t.Fatalf("flips: got %d (%+v), want 3", len(flips), flips)
	}
	if flips[0].setValue != "OFF" || flips[0].armLabel != "//options:feat_off" {
		t.Errorf("FEAT flip: %+v", flips[0])
	}
	if flips[1].setValue != "fast" || flips[1].armLabel != "//options:backend_fast" {
		t.Errorf("BACKEND flip 1: %+v", flips[1])
	}
	if flips[2].setValue != "turbo" || flips[2].armLabel != "//options:backend_turbo" {
		t.Errorf("BACKEND flip 2: %+v", flips[2])
	}
}

func TestEnumSpecOK_RejectsSuffixCollision(t *testing.T) {
	if enumSpecOK("X", "a b", []string{"a b", "a_b"}) {
		t.Errorf("values sanitizing to one suffix must be rejected")
	}
	if !enumSpecOK("X", "a", []string{"a", "b"}) {
		t.Errorf("clean enum rejected")
	}
}

func TestFoldGroups_SplitsByPresenceSignature(t *testing.T) {
	base, off := "//options:feat_on", "//options:feat_off"
	lc := &liftedCells{
		cells: []string{base, off},
		byCell: map[string]map[string]fileapi.Target{
			"everywhere": {base: {Name: "everywhere"}, off: {Name: "everywhere"}},
			"base_only":  {base: {Name: "base_only"}},
			"also_both":  {base: {Name: "also_both"}, off: {Name: "also_both"}},
		},
	}
	groups := lc.foldGroups()
	if len(groups) != 2 {
		t.Fatalf("groups: got %d, want 2 (both-cells + base-only)", len(groups))
	}
	for _, g := range groups {
		switch len(g.cells) {
		case 2:
			if len(g.byCell) != 2 {
				t.Errorf("both-cells group: %v", g.byCell)
			}
		case 1:
			if g.cells[0] != base || len(g.byCell) != 1 {
				t.Errorf("base-only group: cells=%v targets=%v", g.cells, g.byCell)
			}
		default:
			t.Errorf("unexpected group cells: %v", g.cells)
		}
	}
}

// TestGroup2DCells_PartialEnumPresence pins the 2D analogue of
// foldGroups: a target present under a VALUE subset (all configs)
// folds over exactly its present values instead of being dropped —
// dropping it would silently lose its per-value attribute arms.
// Ragged (config-level) holes and single-value targets still drop.
func TestGroup2DCells_PartialEnumPresence(t *testing.T) {
	configs := []string{"Debug", "Release"}
	ref, fast, turbo := "//options:backend_ref", "//options:backend_fast", "//options:backend_turbo"
	valueArms := []string{ref, fast, turbo}
	cellsOver := func(name string, values ...string) map[string]fileapi.Target {
		out := map[string]fileapi.Target{}
		for _, v := range values {
			for _, c := range configs {
				out[lower.Cell2DKey(c, v)] = fileapi.Target{Name: name}
			}
		}
		return out
	}
	byCell := map[string]map[string]fileapi.Target{
		"full":    cellsOver("full", ref, fast, turbo),
		"partial": cellsOver("partial", ref, fast),
		"single":  cellsOver("single", ref),
	}
	// Ragged: present under fast in Debug only (config-level hole).
	byCell["ragged"] = cellsOver("ragged", ref)
	byCell["ragged"][lower.Cell2DKey("Debug", fast)] = fileapi.Target{Name: "ragged"}

	groups := group2DCells(byCell, configs, valueArms)
	if len(groups) != 2 {
		t.Fatalf("groups: got %d, want 2 (full-grid + ref/fast subset): %+v", len(groups), groups)
	}
	for _, g := range groups {
		switch len(g.valueArms) {
		case 3:
			if len(g.byCell) != 1 || g.byCell["full"] == nil {
				t.Errorf("full-grid group: %v", g.byCell)
			}
		case 2:
			if g.valueArms[0] != ref || g.valueArms[1] != fast {
				t.Errorf("subset group arms out of base-first order: %v", g.valueArms)
			}
			if len(g.byCell) != 1 || g.byCell["partial"] == nil {
				t.Errorf("subset group must hold exactly the partial target: %v", g.byCell)
			}
		default:
			t.Errorf("unexpected group: %+v", g)
		}
	}
}

func TestConfigTargetNameSetAndEquality(t *testing.T) {
	cfgs := []fileapi.Configuration{{
		Name: "Release",
		Targets: []fileapi.ConfigTargetRef{
			{Id: "a::@1", Name: "a"}, {Id: "b::@2", Name: "b"},
		},
	}}
	got := configTargetNameSet(cfgs)
	if len(got) != 2 || !got["a"] || !got["b"] {
		t.Fatalf("name set: %v", got)
	}
	if !stringSetsEqual(got, map[string]bool{"a": true, "b": true}) {
		t.Errorf("sets should be equal")
	}
	if stringSetsEqual(got, map[string]bool{"a": true}) || stringSetsEqual(got, map[string]bool{"a": true, "c": true}) {
		t.Errorf("unequal sets reported equal")
	}
}
