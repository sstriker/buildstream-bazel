package bazel

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_SplitsSelectsByFamily pins the multi-axis composition
// contract: arms of different select families (two options' flags
// here) render as SEPARATE select() expressions concatenated with
// `+` — one shared select() would be an "Illegal ambiguous match"
// whenever both flags' conditions hold at once. Single-family
// packages keep the pre-family single-select shape byte-identically.
func TestEmit_SplitsSelectsByFamily(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		SelectArmFamilies: map[string]string{
			"//options:foo_on":       "//options:foo",
			"//options:backend_fast": "//options:backend",
		},
		Targets: []ir.Target{{
			Name: "lib",
			Kind: ir.KindCCLibrary,
			PerPlatform: map[string]map[string][]string{
				"defines": {
					"//options:foo_on":       {"FOO=1"},
					"//options:backend_fast": {"FAST=1"},
				},
			},
		}},
	}
	body, err := Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if got := strings.Count(s, "select({"); got != 2 {
		t.Fatalf("want 2 selects (one per family), got %d:\n%s", got, s)
	}
	if !strings.Contains(s, "}) + select({") {
		t.Errorf("selects not concatenated with +:\n%s", s)
	}
	// Deterministic family order: backend before foo (sorted family key).
	if strings.Index(s, "backend_fast") > strings.Index(s, "foo_on") {
		t.Errorf("family order not deterministic-sorted:\n%s", s)
	}
}

// TestEmit_SingleFamilySingleSelect pins byte-shape stability: with
// one family (or no family map), the single-select form is unchanged.
func TestEmit_SingleFamilySingleSelect(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name: "lib",
			Kind: ir.KindCCLibrary,
			PerPlatform: map[string]map[string][]string{
				"defines": {
					"//config:debug":   {"DBG=1"},
					"//config:release": {"REL=1"},
				},
			},
		}},
	}
	body, err := Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), "select({"); got != 1 {
		t.Fatalf("want 1 select, got %d:\n%s", got, body)
	}
}

// TestEmit_GateSplitsByFamily: two options gating one target emit
// per-family target_compatible_with selects (both matching in one
// select would be ambiguous).
func TestEmit_GateSplitsByFamily(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		SelectArmFamilies: map[string]string{
			"//options:a_off": "//options:a",
			"//options:b_off": "//options:b",
		},
		Targets: []ir.Target{{
			Name: "tool",
			Kind: ir.KindCCBinary,
			Srcs: []string{"t.c"},
			PerPlatform: map[string]map[string][]string{
				"target_compatible_with": {
					"//options:a_off": {"@platforms//:incompatible"},
					"//options:b_off": {"@platforms//:incompatible"},
				},
			},
		}},
	}
	body, err := Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "target_compatible_with = select({") || !strings.Contains(s, "}) + select({") {
		t.Errorf("gate not split per family:\n%s", s)
	}
}
