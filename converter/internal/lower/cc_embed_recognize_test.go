package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// recognizeCcEmbedCase drives recognizeCcEmbed with a vtkEncodeString -P
// command carrying the given extra -D args, against a build statement
// that writes a sibling .h + .cxx. Returns whether the recognizer fired.
func recognizeCcEmbedCase(t *testing.T, extraDArgs string) bool {
	t.Helper()
	cc := newCodegenContext()
	cc.LiftCCEmbed = true
	b := &ninja.Build{Outputs: []string{"/build/x.h", "/build/x.cxx"}}
	cmd := `/usr/bin/cmake "-Dsource_file=/src/x.glsl" "-Doutput_name=x_glsl" ` +
		extraDArgs + ` -P /src/vtkEncodeString.cmake`
	_, ok := recognizeCcEmbed(cc, b, cmd, "vtkEncodeString.cmake", "/src", "/build")
	return ok
}

// TestRecognizeCcEmbed_DeclinesAbiMangle pins that a vtkEncodeString site
// passing a non-empty ABI_MANGLE_* arg is DECLINED (falls through to
// runner/bake) — the cc-embed tool can't reproduce the symbol mangling,
// so lifting it would silently define the wrong symbol. Empty mangle
// args (the common case VTK always emits) must still be recognized.
func TestRecognizeCcEmbed_DeclinesAbiMangle(t *testing.T) {
	// Baseline: empty mangle keys present (as real VTK emits) → recognized.
	if !recognizeCcEmbedCase(t, `"-Dabi_mangle_symbol_begin=" "-Dabi_mangle_symbol_end=" "-Dabi_mangle_header="`) {
		t.Error("empty abi_mangle_* args should still be recognized")
	}
	// Each non-empty mangle arg independently forces a decline.
	for _, arg := range []string{
		`"-Dabi_mangle_symbol_begin=VTK_ABI_NAMESPACE_BEGIN"`,
		`"-Dabi_mangle_symbol_end=VTK_ABI_NAMESPACE_END"`,
		`"-Dabi_mangle_header=vtkABINamespace.h"`,
	} {
		if recognizeCcEmbedCase(t, arg) {
			t.Errorf("non-empty mangle arg %s must force a decline", arg)
		}
	}
}

func TestParseCmakeDashDMap(t *testing.T) {
	cmd := `/usr/bin/cmake "-Dsource_file=/src/x.glsl" "-Doutput_name=x_glsl" -D binary=OFF "-Dexport_symbol=" -P /src/vtkEncodeString.cmake`
	m := parseCmakeDashDMap(cmd)
	if m["source_file"] != "/src/x.glsl" {
		t.Errorf("source_file = %q", m["source_file"])
	}
	if m["output_name"] != "x_glsl" {
		t.Errorf("output_name = %q", m["output_name"])
	}
	if m["binary"] != "OFF" {
		t.Errorf("binary = %q (space-separated -D form)", m["binary"])
	}
	if v, ok := m["export_symbol"]; !ok || v != "" {
		t.Errorf("export_symbol = %q present=%v (empty value should be recorded)", v, ok)
	}
}

func TestPickHeaderSource(t *testing.T) {
	h, s := pickHeaderSource([]string{"sub/x.cxx", "sub/x.h", "junk.txt"})
	if h != "sub/x.h" || s != "sub/x.cxx" {
		t.Errorf("got header=%q source=%q, want sub/x.h, sub/x.cxx", h, s)
	}
	// Only a header (no compilable source) → source empty (recognizer declines).
	if h, s := pickHeaderSource([]string{"only.h"}); h != "only.h" || s != "" {
		t.Errorf("header-only: got header=%q source=%q", h, s)
	}
	// .c counts as a source.
	if _, s := pickHeaderSource([]string{"a.h", "a.c"}); s != "a.c" {
		t.Errorf(".c source: got %q", s)
	}
}
