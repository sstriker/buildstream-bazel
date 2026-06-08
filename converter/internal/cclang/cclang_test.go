package cclang

import "testing"

// TestIsCompiledSource locks the compiled-translation-unit classification both
// lowering and split-emit now share: case-insensitive `.S` handling, the `.sx`
// omission (not a rules_cc source), and headers / extension-less paths as
// non-compiled. Catches regressions like dropping the case-fold or re-adding an
// extension Bazel can't compile.
func TestIsCompiledSource(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"foo.c", true},
		{"foo.cc", true},
		{"foo.cpp", true},
		{"foo.cxx", true},
		{"foo.c++", true},
		{"foo.cu", true},
		{"foo.cl", true},
		{"foo.cppm", true},
		{"foo.ixx", true},
		{"foo.s", true},
		{"foo.asm", true},
		{"kernel/amax.S", true}, // capital-S asm: matched case-insensitively
		{"FOO.CPP", true},       // case-insensitive
		{"foo.sx", false},       // not in rules_cc's ALLOWED_SRC_FILES
		{"foo.h", false},        // header
		{"foo.hpp", false},      // header
		{"foo.inc", false},      // not a compiled TU
		{"noext", false},        // no extension
		{"", false},             // empty path
	}
	for _, c := range cases {
		if got := IsCompiledSource(c.path); got != c.want {
			t.Errorf("IsCompiledSource(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
