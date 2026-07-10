package elementfold

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func twoCells(linux, darwin ir.Package) []Cell {
	return []Cell{
		{Platform: Platform{Name: "linux", Constraints: []string{"@platforms//os:linux"}, SelectKey: "@platforms//os:linux"}, Pkg: &linux},
		{Platform: Platform{Name: "darwin", Constraints: []string{"@platforms//os:darwin"}, SelectKey: "@platforms//os:darwin"}, Pkg: &darwin},
	}
}

// An option arm identical in every cell passes through the fold under
// its own label, and the cells' select-arm family maps union onto the
// merged package.
func TestFold_OptionArmAgreesAcrossCells(t *testing.T) {
	mk := func() ir.Package {
		return ir.Package{
			Name: "p",
			SelectArmFamilies: map[string]string{
				"//options:foo_on": "//options:foo",
			},
			Targets: []ir.Target{{
				Name: "lib", Kind: ir.KindCCLibrary,
				Srcs: []string{"common.c"},
				PerPlatform: map[string]map[string][]string{
					"defines": {"//options:foo_on": {"FOO=1"}},
				},
			}},
		}
	}
	out, err := Fold(twoCells(mk(), mk()))
	if err != nil {
		t.Fatal(err)
	}
	got := out.Targets[0].PerPlatform["defines"]["//options:foo_on"]
	if !reflect.DeepEqual(got, []string{"FOO=1"}) {
		t.Errorf("pass-through arm: %v", got)
	}
	if out.SelectArmFamilies["//options:foo_on"] != "//options:foo" {
		t.Errorf("family map not unioned: %v", out.SelectArmFamilies)
	}
	if len(out.Targets) != 1 {
		t.Errorf("no groups expected: %v", out.Targets)
	}
}

// An option arm item present in ONE cell only is option x platform-
// conditional: it routes through a selects.config_setting_group
// AND-arm emitted into the merged package, registered under the
// option family's +platform group family.
func TestFold_OptionArmDivergesAcrossCells(t *testing.T) {
	linux := ir.Package{
		Name:              "p",
		SelectArmFamilies: map[string]string{"//options:foo_on": "//options:foo"},
		Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			Srcs: []string{"common.c"},
			PerPlatform: map[string]map[string][]string{
				"defines": {"//options:foo_on": {"FOO_LINUX=1"}},
			},
		}},
	}
	darwin := ir.Package{
		Name:              "p",
		SelectArmFamilies: map[string]string{"//options:foo_on": "//options:foo"},
		Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			Srcs: []string{"common.c"},
		}},
	}
	out, err := Fold(twoCells(linux, darwin))
	if err != nil {
		t.Fatal(err)
	}
	groupLabel := ":linux_and_options_foo_on"
	got := out.Targets[0].PerPlatform["defines"][groupLabel]
	if !reflect.DeepEqual(got, []string{"FOO_LINUX=1"}) {
		t.Fatalf("AND arm: %v (PerPlatform: %v)", got, out.Targets[0].PerPlatform)
	}
	if _, ok := out.Targets[0].PerPlatform["defines"]["//options:foo_on"]; ok {
		t.Errorf("divergent item must not stay on the plain option arm")
	}
	var group *ir.Target
	for i := range out.Targets {
		if out.Targets[i].Kind == ir.KindConfigSettingGroup {
			group = &out.Targets[i]
		}
	}
	if group == nil {
		t.Fatalf("config_setting_group target missing: %v", out.Targets)
	}
	if group.Name != "linux_and_options_foo_on" ||
		!reflect.DeepEqual(group.GroupMatchAll, []string{"@platforms//os:linux", "//options:foo_on"}) {
		t.Errorf("group: %+v", group)
	}
	if fam := out.SelectArmFamilies[groupLabel]; fam != "//options:foo+platform" {
		t.Errorf("group family: %q", fam)
	}
}

// Cells registering one label under different families are incoherent.
func TestFold_FamilyConflictErrors(t *testing.T) {
	linux := ir.Package{Name: "p", SelectArmFamilies: map[string]string{"//options:x_on": "//options:x"},
		Targets: []ir.Target{{Name: "lib", Kind: ir.KindCCLibrary}}}
	darwin := ir.Package{Name: "p", SelectArmFamilies: map[string]string{"//options:x_on": "//options:y"},
		Targets: []ir.Target{{Name: "lib", Kind: ir.KindCCLibrary}}}
	if _, err := Fold(twoCells(linux, darwin)); err == nil {
		t.Fatalf("family conflict must error")
	}
}

// Order-sensitive pre-existing arms: agreement passes through; a
// per-cell divergent copts arm routes each cell's verbatim sequence
// through its AND-group.
func TestFold_OrderSensitiveOptionArms(t *testing.T) {
	linux := ir.Package{
		Name:              "p",
		SelectArmFamilies: map[string]string{"//options:foo_on": "//options:foo"},
		Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			PerPlatform: map[string]map[string][]string{
				"copts": {"//options:foo_on": {"-O2", "-fno-plt"}},
			},
		}},
	}
	darwin := ir.Package{
		Name:              "p",
		SelectArmFamilies: map[string]string{"//options:foo_on": "//options:foo"},
		Targets: []ir.Target{{
			Name: "lib", Kind: ir.KindCCLibrary,
			PerPlatform: map[string]map[string][]string{
				"copts": {"//options:foo_on": {"-O2"}},
			},
		}},
	}
	out, err := Fold(twoCells(linux, darwin))
	if err != nil {
		t.Fatal(err)
	}
	copts := out.Targets[0].PerPlatform["copts"]
	if got := copts[":linux_and_options_foo_on"]; !reflect.DeepEqual(got, []string{"-O2", "-fno-plt"}) {
		t.Errorf("linux AND arm: %v", got)
	}
	if got := copts[":darwin_and_options_foo_on"]; !reflect.DeepEqual(got, []string{"-O2"}) {
		t.Errorf("darwin AND arm: %v", got)
	}
	if _, ok := copts["//options:foo_on"]; ok {
		t.Errorf("divergent order-sensitive arm must not pass through: %v", copts)
	}
}

// TestGroupSink_SanitizeCollisionDisambiguates: distinct
// (SelectKey, armLabel) pairs whose names sanitize identically get
// distinct group labels rather than silently sharing one.
func TestGroupSink_SanitizeCollisionDisambiguates(t *testing.T) {
	sink := &groupSink{families: map[string]string{}, defs: map[string]ir.Target{}}
	a := sink.group(Cell{Platform: Platform{Name: "linux", SelectKey: "@platforms//os:linux"}}, "//options:foo_on")
	b := sink.group(Cell{Platform: Platform{Name: "Linux", SelectKey: "//platforms:linux_x86_64"}}, "//options:foo_on")
	if a == b {
		t.Fatalf("colliding sanitized names must disambiguate: %q vs %q", a, b)
	}
	// Same pair reuses the same label.
	if again := sink.group(Cell{Platform: Platform{Name: "linux", SelectKey: "@platforms//os:linux"}}, "//options:foo_on"); again != a {
		t.Errorf("same pair must reuse: %q vs %q", again, a)
	}
	if got := sink.defs[b].GroupMatchAll[0]; got != "//platforms:linux_x86_64" {
		t.Errorf("disambiguated group's match_all: %v", sink.defs[b].GroupMatchAll)
	}
}
