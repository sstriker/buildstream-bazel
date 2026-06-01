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
