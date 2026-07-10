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
	body := string(optionsettings.Emit([]optionsettings.Option{{Name: "Foo_Feature", Default: "True"}}, nil))
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

// TestOptionValueCellLabel_MatchesOptionSettingsEmit is the enum
// sibling of the parity test above: the fold's per-value arm labels
// must name config_settings the emitted //options package declares.
func TestOptionValueCellLabel_MatchesOptionSettingsEmit(t *testing.T) {
	values := []string{"Ref", "FAST 2"}
	suffixes := map[string]string{}
	for _, v := range values {
		suffixes[v] = SanitizeOptionValue(v)
	}
	body := string(optionsettings.Emit([]optionsettings.Option{{
		Name: "Backend", Default: "Ref", Values: values, ValueSuffixes: suffixes,
	}}, nil))
	for _, v := range values {
		label := OptionValueCellLabel("Backend", v)
		name := strings.TrimPrefix(label, "//options:")
		if !strings.Contains(body, "name = \""+name+"\"") {
			t.Errorf("emitted //options package lacks config_setting %q for value %q:\n%s", name, v, body)
		}
	}
}

func TestSanitizeOptionValue(t *testing.T) {
	for in, want := range map[string]string{
		"Ref":    "ref",
		"FAST 2": "fast_2",
		"a.b-c":  "a.b-c",
		"x/y":    "x_y",
	} {
		if got := SanitizeOptionValue(in); got != want {
			t.Errorf("SanitizeOptionValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplyOptionFold_EnumThreeCells pins the multi-cell (enum)
// fold: per-value defines land under each value's arm; the value
// whose cell agrees with every other cell contributes baseline only.
func TestApplyOptionFold_EnumThreeCells(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	mk := func(def string) fileapi.Target {
		defs := []fileapi.CompileDefine{{Define: "COMMON=1"}}
		if def != "" {
			defs = append(defs, fileapi.CompileDefine{Define: def})
		}
		return fileapi.Target{Name: "foo", CompileGroups: []fileapi.CompileGroup{{Language: "C", Defines: defs}}}
	}
	ref := OptionValueCellLabel("BACKEND", "ref")
	fast := OptionValueCellLabel("BACKEND", "fast")
	turbo := OptionValueCellLabel("BACKEND", "turbo")
	byCell := map[string]map[string]fileapi.Target{
		"foo": {ref: mk(""), fast: mk("USE_FAST=1"), turbo: mk("USE_TURBO=1")},
	}
	lifted := ApplyOptionFold(pkg, byCell, []string{ref, fast, turbo}, "", "", nil)
	if len(lifted) != 1 {
		t.Fatalf("lifted = %v", lifted)
	}
	defines := pkg.Targets[0].PerPlatform["defines"]
	if got := defines["//options:backend_fast"]; len(got) != 1 || got[0] != "USE_FAST=1" {
		t.Errorf("fast arm: %v", got)
	}
	if got := defines["//options:backend_turbo"]; len(got) != 1 || got[0] != "USE_TURBO=1" {
		t.Errorf("turbo arm: %v", got)
	}
	if got, ok := defines["//options:backend_ref"]; ok {
		t.Errorf("ref arm should be empty/absent (COMMON=1 is baseline): %v", got)
	}
}

// TestApplyContentBakes_FamilyMergeAndConflict pins the shared
// content-bake fold's family rules: bodies keyed by final arm labels
// land on WriteFileContentByConfig, merging with arms an earlier
// SAME-family fold placed — but a target whose existing arms belong
// to a DIFFERENT family is skipped (a content select is one select;
// two families' conditions can match simultaneously and bodies
// aren't additive).
func TestApplyContentBakes_FamilyMergeAndConflict(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:             "cfg_h",
		Kind:             ir.KindWriteFile,
		WriteFileOut:     "cfg.h",
		WriteFileContent: []string{"#define BACKEND ref"},
	}}}
	// First fold: backend arm applies + registers its family.
	applied, skipped := ApplyContentBakes(pkg, map[string]map[string][]byte{
		"cfg.h": {"//options:backend_fast": []byte("#define BACKEND fast")},
	}, "", "", "", "cmake-codegen-per-option-content", "//options:backend")
	if len(applied) != 1 || applied[0] != "cfg_h" || len(skipped) != 0 {
		t.Fatalf("first fold: applied=%v skipped=%v", applied, skipped)
	}
	// Same family again (another value of the same option): merges.
	applied, skipped = ApplyContentBakes(pkg, map[string]map[string][]byte{
		"cfg.h": {"//options:backend_turbo": []byte("#define BACKEND turbo")},
	}, "", "", "", "cmake-codegen-per-option-content", "//options:backend")
	if len(applied) != 1 || len(skipped) != 0 {
		t.Fatalf("same-family fold: applied=%v skipped=%v", applied, skipped)
	}
	byCfg := pkg.Targets[0].WriteFileContentByConfig
	if got := byCfg["//options:backend_fast"]; len(got) != 1 || got[0] != "#define BACKEND fast" {
		t.Errorf("fast arm: %v", got)
	}
	if got := byCfg["//options:backend_turbo"]; len(got) != 1 || got[0] != "#define BACKEND turbo" {
		t.Errorf("turbo arm (same-family merge): %v", got)
	}
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-codegen-per-option-content") {
		t.Errorf("audit tag missing: %v", pkg.Targets[0].Tags)
	}
	// Different family (another option): skipped, arms unchanged.
	applied, skipped = ApplyContentBakes(pkg, map[string]map[string][]byte{
		"cfg.h": {"//options:feat_off": []byte("#define BACKEND none")},
	}, "", "", "", "cmake-codegen-per-option-content", "//options:feat")
	if len(applied) != 0 || len(skipped) != 1 || skipped[0] != "cfg_h" {
		t.Fatalf("cross-family fold: applied=%v skipped=%v", applied, skipped)
	}
	if _, ok := pkg.Targets[0].WriteFileContentByConfig["//options:feat_off"]; ok {
		t.Errorf("cross-family arm must not land: %v", pkg.Targets[0].WriteFileContentByConfig)
	}
}

func TestGateTargetExistence(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "extra_tool", Kind: ir.KindCCBinary},
		{Name: "always", Kind: ir.KindCCLibrary},
	}}
	gated := GateTargetExistence(pkg, map[string][]string{
		"extra_tool": {"//options:build_extra_tool_off"},
		"not_in_pkg": {"//options:x_off"},
	})
	if !reflect.DeepEqual(gated, []string{"extra_tool"}) {
		t.Fatalf("gated = %v, want [extra_tool]", gated)
	}
	arms := pkg.Targets[0].PerPlatform["target_compatible_with"]
	if got := arms["//options:build_extra_tool_off"]; len(got) != 1 || got[0] != IncompatibleLabel {
		t.Errorf("gate arm: %v", got)
	}
	if pkg.Targets[1].PerPlatform != nil {
		t.Errorf("ungated target should stay untouched: %v", pkg.Targets[1].PerPlatform)
	}
	// Idempotent: re-gating the same arm must not duplicate the label.
	GateTargetExistence(pkg, map[string][]string{"extra_tool": {"//options:build_extra_tool_off"}})
	if got := pkg.Targets[0].PerPlatform["target_compatible_with"]["//options:build_extra_tool_off"]; len(got) != 1 {
		t.Errorf("gate arm duplicated: %v", got)
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
