package bazel

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// A KindCCEmbed target renders the cc_embed rule + its load line; optional
// attributes are omitted at their defaults.
func TestEmitCCEmbed(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:       "gen_shader_glsl_h",
			Kind:       ir.KindCCEmbed,
			Tags:       []string{"cmake-codegen-cc-embed"},
			Visibility: []string{"//visibility:private"},
			CCEmbed: &ir.CCEmbedSpec{
				Src:       "shader.glsl",
				Symbol:    "shader_glsl",
				OutHeader: "shader_glsl.h",
				OutSource: "shader_glsl.cxx",
			},
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(out)
	t.Logf("rendered:\n%s", s)

	for _, want := range []string{
		`load("@rules_buildstream_bazel//rules:cc_embed.bzl", "cc_embed")`,
		`cc_embed(`,
		`name = "gen_shader_glsl_h"`,
		`src = "shader.glsl"`,
		`symbol = "shader_glsl"`,
		`out_header = "shader_glsl.h"`,
		`out_source = "shader_glsl.cxx"`,
		`tool = "//tools:cc-embed"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// Default-valued optionals are omitted.
	for _, notWant := range []string{"binary =", "nul_terminate =", "export_symbol =", "export_header ="} {
		if strings.Contains(s, notWant) {
			t.Errorf("default-valued attr %q should be omitted; got:\n%s", notWant, s)
		}
	}
}

// Binary + export attributes render when set.
func TestEmitCCEmbed_BinaryAndExport(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name: "gen_blob_h",
			Kind: ir.KindCCEmbed,
			CCEmbed: &ir.CCEmbedSpec{
				Src:          "blob.bin",
				Symbol:       "blob",
				OutHeader:    "blob.h",
				OutSource:    "blob.cxx",
				Binary:       true,
				NulTerminate: true,
				ExportSymbol: "MYLIB_EXPORT",
				ExportHeader: "mylib/export.h",
			},
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"binary = True",
		"nul_terminate = True",
		`export_symbol = "MYLIB_EXPORT"`,
		`export_header = "mylib/export.h"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}
