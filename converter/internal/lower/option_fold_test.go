package lower

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/optionsettings"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// optionCells builds the two-cell byCell map ApplyOptionFold
// consumes for a single target named "foo".
func optionCells(base, flip fileapi.Target) (map[string]map[string]fileapi.Target, []string) {
	on := OptionCellLabel("FOO_FEATURE", true)
	off := OptionCellLabel("FOO_FEATURE", false)
	return map[string]map[string]fileapi.Target{
		"foo": {on: base, off: flip},
	}, []string{on, off}
}

func TestApplyOptionFold_PopulatesPerPlatform(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	base := fileapi.Target{
		Name: "foo",
		CompileGroups: []fileapi.CompileGroup{{
			Language: "C",
			Defines:  []fileapi.CompileDefine{{Define: "COMMON=1"}, {Define: "FOO_FEATURE=1"}},
		}},
	}
	flip := fileapi.Target{
		Name: "foo",
		CompileGroups: []fileapi.CompileGroup{{
			Language: "C",
			Defines:  []fileapi.CompileDefine{{Define: "COMMON=1"}},
		}},
	}
	byCell, cells := optionCells(base, flip)
	lifted := ApplyOptionFold(pkg, byCell, cells, "", "", nil)
	if !reflect.DeepEqual(lifted, []string{"foo"}) {
		t.Fatalf("lifted = %v, want [foo]", lifted)
	}
	defines := pkg.Targets[0].PerPlatform["defines"]
	if defines == nil {
		t.Fatalf("defines arms missing: %v", pkg.Targets[0].PerPlatform)
	}
	if got := defines["//options:foo_feature_on"]; len(got) != 1 || got[0] != "FOO_FEATURE=1" {
		t.Errorf("on arm: %v", got)
	}
	if got, ok := defines["//options:foo_feature_off"]; ok {
		t.Errorf("off arm should not exist (COMMON=1 is baseline): %v", got)
	}
}

func TestApplyOptionFold_SrcsAndDeps(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	base := fileapi.Target{
		Name:         "foo",
		Sources:      []fileapi.TargetSource{{Path: "common.c"}, {Path: "feature.c"}},
		Dependencies: []fileapi.TargetDependency{{Id: "featlib::@1"}},
	}
	flip := fileapi.Target{
		Name:    "foo",
		Sources: []fileapi.TargetSource{{Path: "common.c"}},
	}
	byCell, cells := optionCells(base, flip)
	lifted := ApplyOptionFold(pkg, byCell, cells, "", "", map[string]string{"featlib::@1": "featlib"})
	if len(lifted) != 1 {
		t.Fatalf("lifted = %v, want one target", lifted)
	}
	srcs := pkg.Targets[0].PerPlatform["srcs"]
	if got := srcs["//options:foo_feature_on"]; len(got) != 1 || got[0] != "feature.c" {
		t.Errorf("srcs on arm: %v", got)
	}
	deps := pkg.Targets[0].PerPlatform["deps"]
	if got := deps["//options:foo_feature_on"]; len(got) != 1 || got[0] != ":featlib" {
		t.Errorf("deps on arm: %v", got)
	}
}

func TestApplyOptionFold_NoDeltasNoLift(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	same := fileapi.Target{
		Name: "foo",
		CompileGroups: []fileapi.CompileGroup{{
			Language: "C",
			Defines:  []fileapi.CompileDefine{{Define: "COMMON=1"}},
		}},
	}
	byCell, cells := optionCells(same, same)
	if lifted := ApplyOptionFold(pkg, byCell, cells, "", "", nil); len(lifted) != 0 {
		t.Errorf("identical cells should lift nothing; got %v", lifted)
	}
	if pkg.Targets[0].PerPlatform != nil {
		t.Errorf("PerPlatform should stay nil; got %v", pkg.Targets[0].PerPlatform)
	}
}

func TestApplyOptionFold_DedupsFlatBaseline(t *testing.T) {
	// A define the primary lower already put in the flat attribute
	// that turns out to be option-conditional must move to its arm.
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary, Defines: []string{"FOO_FEATURE=1"}}},
	}
	base := fileapi.Target{
		Name: "foo",
		CompileGroups: []fileapi.CompileGroup{{
			Language: "C",
			Defines:  []fileapi.CompileDefine{{Define: "FOO_FEATURE=1"}},
		}},
	}
	flip := fileapi.Target{Name: "foo", CompileGroups: []fileapi.CompileGroup{{Language: "C"}}}
	byCell, cells := optionCells(base, flip)
	ApplyOptionFold(pkg, byCell, cells, "", "", nil)
	if got := pkg.Targets[0].Defines; len(got) != 0 {
		t.Errorf("flat Defines should have been deduped against the arm; got %v", got)
	}
}

// TestOptionCellLabel_MatchesOptionSettingsEmit pins that the fold's
// arm labels and the emitted //options package agree on naming —
// the same parity contract TestConfigLabel_MatchesConfigSettingsEmit
// pins for the multi-config fold.
func TestOptionCellLabel_MatchesOptionSettingsEmit(t *testing.T) {
	body := string(optionsettings.Emit([]optionsettings.Option{{Name: "Foo_Feature", Default: true}}))
	for _, on := range []bool{true, false} {
		label := OptionCellLabel("Foo_Feature", on)
		name := strings.TrimPrefix(label, "//options:")
		if name == label {
			t.Fatalf("label %q not under //options:", label)
		}
		if !strings.Contains(body, "name = \""+name+"\"") {
			t.Errorf("emitted //options package lacks config_setting %q:\n%s", name, body)
		}
	}
}

func TestAnnotateLiftedOptions_RelocatesLiftedEntries(t *testing.T) {
	pkg := &ir.Package{HeaderComments: []string{
		"some attribution",
		"",
		optionsBakedHeader,
		"  - BAR_OTHER = OFF (other toggle)",
		"  - FOO_FEATURE = ON (feature toggle)",
		"trailing line",
	}}
	AnnotateLiftedOptions(pkg, map[string]string{"FOO_FEATURE": "//options:foo_feature"})
	want := []string{
		"some attribution",
		"",
		optionsBakedHeader,
		"  - BAR_OTHER = OFF (other toggle)",
		"",
		optionsLiftedHeader,
		"  - FOO_FEATURE (//options:foo_feature, default from this convert)",
		"trailing line",
	}
	if !reflect.DeepEqual(pkg.HeaderComments, want) {
		t.Errorf("HeaderComments:\n got %q\nwant %q", pkg.HeaderComments, want)
	}
}

func TestAnnotateLiftedOptions_DropsEmptiedBakedBlock(t *testing.T) {
	pkg := &ir.Package{HeaderComments: []string{
		"",
		optionsBakedHeader,
		"  - FOO_FEATURE = ON (feature toggle)",
	}}
	AnnotateLiftedOptions(pkg, map[string]string{"FOO_FEATURE": "//options:foo_feature"})
	want := []string{
		"",
		optionsLiftedHeader,
		"  - FOO_FEATURE (//options:foo_feature, default from this convert)",
	}
	if !reflect.DeepEqual(pkg.HeaderComments, want) {
		t.Errorf("HeaderComments:\n got %q\nwant %q", pkg.HeaderComments, want)
	}
}
