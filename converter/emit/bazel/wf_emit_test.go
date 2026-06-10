package bazel

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestEmitWriteFileOrderAndEscaping(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:             "gen_x",
			Kind:             ir.KindWriteFile,
			WriteFileOut:     "x.txt",
			WriteFileContent: []string{"zebra", "alpha", `quote " and \ back`, ""},
			WriteFileNewline: "unix",
			Tags:             []string{"cmake-codegen"},
			Visibility:       []string{"//visibility:private"},
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(out)
	t.Logf("rendered:\n%s", s)
	if !strings.Contains(s, `load("@bazel_skylib//rules:write_file.bzl", "write_file")`) {
		t.Error("missing skylib write_file load")
	}
	// Order must be preserved (file body, not a set) — zebra before alpha.
	zi := strings.Index(s, `"zebra"`)
	ai := strings.Index(s, `"alpha"`)
	if zi < 0 || ai < 0 || zi > ai {
		t.Errorf("content order not preserved (buildifier sorted it?): zebra@%d alpha@%d", zi, ai)
	}
	if !strings.Contains(s, `"quote \" and \\ back"`) {
		t.Errorf("special chars not escaped as expected")
	}
}

// A write_file carrying per-build-type bodies (WriteFileContentByConfig)
// renders content as a pure select() over the config_setting labels, with
// the primary configure's body as the //conditions:default arm and line
// order preserved inside each arm — the per-config configure_file bake
// shape (LLVM's abi-breaking.h: ABI_BREAKING_CHECKS on for Debug only).
func TestEmitWriteFile_PerConfigContentSelect(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:             "gen_cfg",
			Kind:             ir.KindWriteFile,
			WriteFileOut:     "cfg.h",
			WriteFileContent: []string{"#define CHECKS 0", "// tail"},
			WriteFileContentByConfig: map[string][]string{
				"//config:debug":   {"#define CHECKS 1", "// tail"},
				"//config:release": {"#define CHECKS 0", "// tail"},
			},
			WriteFileNewline: "unix",
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(out)
	t.Logf("rendered:\n%s", s)
	for _, want := range []string{
		"content = select({",
		`"//config:debug": [`,
		`"#define CHECKS 1"`,
		`"//config:release": [`,
		`"//conditions:default": [`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in rendered write_file:\n%s", want, s)
		}
	}
	// The default arm carries the primary configure's body.
	di := strings.Index(s, `"//conditions:default"`)
	if di < 0 || !strings.Contains(s[di:], `"#define CHECKS 0"`) {
		t.Errorf("default arm should carry the primary body:\n%s", s)
	}
}
