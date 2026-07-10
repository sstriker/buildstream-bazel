package optionsettings

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmit_Empty(t *testing.T) {
	if got := Emit(nil); got != nil {
		t.Errorf("Emit(nil) = %q, want nil", got)
	}
}

func TestEmit_Shape(t *testing.T) {
	body := Emit([]Option{{Name: "Zeta_Feature", Default: false}, {Name: "ALPHA", Default: true}})
	s := string(body)
	for _, want := range []string{
		`load("@bazel_skylib//rules:common_settings.bzl", "bool_flag")`,
		"bool_flag(\n    name = \"alpha\",\n    build_setting_default = True,",
		"bool_flag(\n    name = \"zeta_feature\",\n    build_setting_default = False,",
		"config_setting(\n    name = \"alpha_on\",\n    flag_values = {\":alpha\": \"True\"},",
		"config_setting(\n    name = \"alpha_off\",\n    flag_values = {\":alpha\": \"False\"},",
		"config_setting(\n    name = \"zeta_feature_on\",",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// Sorted: alpha before zeta_feature.
	if strings.Index(s, "\"alpha\"") > strings.Index(s, "\"zeta_feature\"") {
		t.Errorf("options not sorted:\n%s", s)
	}
}

func TestEmit_DedupAndStability(t *testing.T) {
	in := []Option{{Name: "Foo", Default: true}, {Name: "FOO", Default: false}}
	a, b := Emit(in), Emit(in)
	if !bytes.Equal(a, b) {
		t.Errorf("Emit not byte-stable")
	}
	// First occurrence's default wins for a case-colliding dup.
	if !strings.Contains(string(a), "build_setting_default = True") {
		t.Errorf("dup should keep first default:\n%s", a)
	}
	if strings.Count(string(a), "bool_flag(") != 1 {
		t.Errorf("dup should emit one flag:\n%s", a)
	}
}
