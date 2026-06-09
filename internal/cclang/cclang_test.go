package cclang

import (
	"path/filepath"
	"strings"
	"testing"
)

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

// TestIsHeader locks the header classification shared by lowering (route-to-hdrs)
// and build-cc-index (gazelle indexing): the plain header exts, the
// template-impl set (.inl/.txx/.tcc/.ipp), the x-macro/.inc idioms, `.h++`,
// case-insensitivity, and compiled sources / extension-less paths as non-headers.
func TestIsHeader(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"foo.h", true},
		{"foo.hh", true},
		{"foo.hpp", true},
		{"foo.hxx", true},
		{"foo.h++", true},
		{"foo.inl", true},
		{"foo.def", true}, // x-macro (LLVM)
		{"foo.inc", true}, // textual-include fragment
		{"foo.txx", true}, // template-impl (VTK)
		{"foo.tcc", true},
		{"foo.ipp", true},
		{"FOO.HPP", true}, // case-insensitive
		{"foo.cc", false}, // compiled source
		{"foo.c", false},
		{"foo.sx", false},
		{"foo.cuh", false}, // CUDA header: deliberately a caller-local special-case, not here
		{"noext", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsHeader(c.path); got != c.want {
			t.Errorf("IsHeader(%q) = %v, want %v", c.path, got, c.want)
		}
		// IsHeaderExt is the lowercased-ext form of the same predicate.
		if got := IsHeaderExt(strings.ToLower(filepath.Ext(c.path))); got != c.want {
			t.Errorf("IsHeaderExt(ext of %q) = %v, want %v", c.path, got, c.want)
		}
	}
}
