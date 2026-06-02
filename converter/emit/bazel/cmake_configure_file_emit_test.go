package bazel

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// When a file(GENERATE) genex literal resolves to different bytes per build
// config, the lift sets GenexValuesPerConfig and the emitter renders the
// genex_values attribute as a select() over the //config:<name> arms (the
// rule's string_dict attr is configurable, so Bazel picks the active config's
// map — no rule change needed).
func TestEmitCMakeConfigureFile_GenexValuesPerConfigSelect(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:       "gen_tag_h",
			Kind:       ir.KindCMakeConfigureFile,
			Tags:       []string{"cmake-codegen"},
			Visibility: []string{"//visibility:private"},
			CMakeConfigureFile: &ir.CMakeConfigureFileSpec{
				Out:     "tag.h",
				Content: "#define TAG \"$<CONFIG>\"\n",
				Values:  map[string]string{},
				Tool:    "//tools:cmake-configure-file",
				GenexValuesPerConfig: map[string]map[string]string{
					"//config:debug":   {"$<CONFIG>": "Debug"},
					"//config:release": {"$<CONFIG>": "Release"},
				},
			},
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(out)
	t.Logf("rendered:\n%s", s)

	if !strings.Contains(s, "genex_values = select({") {
		t.Errorf("genex_values should render as a select(); got:\n%s", s)
	}
	for _, want := range []string{
		`"//config:debug"`,
		`"//config:release"`,
		`"$<CONFIG>": "Debug"`,
		`"$<CONFIG>": "Release"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("select() missing %q; got:\n%s", want, s)
		}
	}
	if !strings.Contains(s, `load("@rules_buildstream_bazel//rules:cmake_configure_file.bzl", "cmake_configure_file")`) {
		t.Error("missing cmake_configure_file load")
	}
	// The select() arms must NOT leave a stray flat genex_values dict.
	if strings.Count(s, "genex_values =") != 1 {
		t.Errorf("expected exactly one genex_values attribute; got:\n%s", s)
	}
}

// TestEmitCMakeConfigureFile_StampValues renders the VCS-stamp lift: the
// stamp_values attribute (template var -> workspace-status key) appears as
// a readable string_dict, alongside the baked values fallback.
func TestEmitCMakeConfigureFile_StampValues(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:       "gen_version_h",
			Kind:       ir.KindCMakeConfigureFile,
			Tags:       []string{"cmake-codegen"},
			Visibility: []string{"//visibility:private"},
			CMakeConfigureFile: &ir.CMakeConfigureFileSpec{
				Out:         "version.h",
				Template:    "src/version.h.in",
				Values:      map[string]string{"GIT_SHA": "abc123"},
				StampValues: map[string]string{"GIT_SHA": "STABLE_GIT_SHA"},
				Tool:        "//tools:cmake-configure-file",
				AtOnly:      true,
			},
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`stamp_values = {`,
		`"GIT_SHA": "STABLE_GIT_SHA"`,
		`values = {`,
		`"GIT_SHA": "abc123"`, // baked fallback stays
	} {
		if !strings.Contains(s, want) {
			t.Errorf("emit missing %q; got:\n%s", want, s)
		}
	}
}

// TestEmitCMakeConfigureFile_NoStampValues confirms the stamp_values attr
// is omitted entirely for the common configure_file with no stamp var.
func TestEmitCMakeConfigureFile_NoStampValues(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:       "gen_cfg_h",
			Kind:       ir.KindCMakeConfigureFile,
			Visibility: []string{"//visibility:private"},
			CMakeConfigureFile: &ir.CMakeConfigureFileSpec{
				Out:      "cfg.h",
				Template: "cfg.h.in",
				Values:   map[string]string{"X": "1"},
				Tool:     "//tools:cmake-configure-file",
			},
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(string(out), "stamp_values") {
		t.Errorf("stamp_values should be omitted when empty; got:\n%s", out)
	}
}
