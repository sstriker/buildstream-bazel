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
