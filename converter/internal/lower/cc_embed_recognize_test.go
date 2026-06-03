package lower

import "testing"

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
