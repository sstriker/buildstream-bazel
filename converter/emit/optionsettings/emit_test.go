package optionsettings

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmit_Empty(t *testing.T) {
	if got := Emit(nil, nil); got != nil {
		t.Errorf("Emit(nil, nil) = %q, want nil", got)
	}
}

func TestEmit_BoolShape(t *testing.T) {
	body := Emit([]Option{{Name: "Zeta_Feature", Default: "False"}, {Name: "ALPHA", Default: "True"}}, nil)
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
	// No string_flag load for a bool-only set (the header prose may
	// mention it; the load line must not).
	if strings.Contains(s, `"string_flag"`) {
		t.Errorf("bool-only emit should not load string_flag:\n%s", s)
	}
}

func TestEmit_EnumShape(t *testing.T) {
	body := Emit([]Option{{
		Name:          "Backend",
		Default:       "Ref",
		Values:        []string{"Ref", "FAST-2"},
		ValueSuffixes: map[string]string{"Ref": "ref", "FAST-2": "fast-2"},
	}}, nil)
	s := string(body)
	for _, want := range []string{
		`load("@bazel_skylib//rules:common_settings.bzl", "string_flag")`,
		"string_flag(\n    name = \"backend\",\n    build_setting_default = \"Ref\",",
		"values = [\n        \"Ref\",\n        \"FAST-2\",\n    ],",
		"config_setting(\n    name = \"backend_ref\",\n    flag_values = {\":backend\": \"Ref\"},",
		"config_setting(\n    name = \"backend_fast-2\",\n    flag_values = {\":backend\": \"FAST-2\"},",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"bool_flag"`) {
		t.Errorf("enum-only emit should not load bool_flag:\n%s", s)
	}
}

func TestEmit_MixedLoads(t *testing.T) {
	body := Emit([]Option{
		{Name: "feat", Default: "True"},
		{Name: "backend", Default: "a", Values: []string{"a", "b"}, ValueSuffixes: map[string]string{"a": "a", "b": "b"}},
	}, nil)
	if !strings.Contains(string(body), `load("@bazel_skylib//rules:common_settings.bzl", "bool_flag", "string_flag")`) {
		t.Errorf("mixed set should load both symbols:\n%s", body)
	}
}

// TestEmit_Groups pins the 2D fold's AND-group rendering: skylib's
// selects.config_setting_group per group, deduplicated + sorted,
// with the selects.bzl load present only when groups exist.
func TestEmit_Groups(t *testing.T) {
	body := Emit([]Option{{Name: "feat", Default: "True"}}, []Group{
		{Name: "debug_and_feat_on", MatchAll: []string{"//config:debug", "//options:feat_on"}},
		{Name: "debug_and_feat_on", MatchAll: []string{"//config:debug", "//options:feat_on"}}, // dup
	})
	s := string(body)
	for _, want := range []string{
		`load("@bazel_skylib//lib:selects.bzl", "selects")`,
		"selects.config_setting_group(\n    name = \"debug_and_feat_on\",",
		"match_all = [\n        \"//config:debug\",\n        \"//options:feat_on\",\n    ],",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Count(s, "config_setting_group(") != 1 {
		t.Errorf("dup group not deduplicated:\n%s", s)
	}
}

func TestEmit_DedupAndStability(t *testing.T) {
	in := []Option{{Name: "Foo", Default: "True"}, {Name: "FOO", Default: "False"}}
	a, b := Emit(in, nil), Emit(in, nil)
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
