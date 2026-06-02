package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_BoolFlagAndConfigSetting renders the lifted-feature-probe
// pair (a configure HAVE_X probe -> an overridable build setting) and
// checks the skylib load + the two rules.
func TestEmit_BoolFlagAndConfigSetting(t *testing.T) {
	pkg := &ir.Package{Name: "demo", Targets: []ir.Target{
		{
			Name: "have_zlib", Kind: ir.KindBoolFlag, BoolFlagDefault: true,
			Tags: []string{"cmake-codegen-probe-option"}, Visibility: []string{"//visibility:public"},
		},
		{
			Name: "have_zlib_enabled", Kind: ir.KindConfigSetting,
			ConfigSettingFlag: ":have_zlib", ConfigSettingValue: "True",
			Visibility: []string{"//visibility:public"},
		},
	}}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		`load("@bazel_skylib//rules:common_settings.bzl", "bool_flag")`,
		`bool_flag(`,
		`name = "have_zlib"`,
		`build_setting_default = True`,
		`tags = ["cmake-codegen-probe-option"]`,
		`config_setting(`,
		`name = "have_zlib_enabled"`,
		`":have_zlib": "True"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("emit missing %q\n--- got ---\n%s", want, s)
		}
	}
}

// TestEmit_BoolFlagDefaultFalse pins the false-default rendering (a probe
// whose value cmake captured as off, or wasn't captured at all).
func TestEmit_BoolFlagDefaultFalse(t *testing.T) {
	pkg := &ir.Package{Name: "demo", Targets: []ir.Target{
		{Name: "have_x", Kind: ir.KindBoolFlag, BoolFlagDefault: false},
	}}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(string(got), "build_setting_default = False") {
		t.Errorf("expected default False; got:\n%s", got)
	}
}
