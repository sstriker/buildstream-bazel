// Package cclang holds C/C++ source-language facts shared across the converter's
// lowering and Bazel-emission halves, so classifiers that must agree can't drift.
// It is a leaf package (stdlib-only), importable by both converter/internal/lower
// and converter/emit/bazel without a cycle.
package cclang

import (
	"path/filepath"
	"strings"
)

// compiledSourceExts are the extensions Bazel/rules_cc compile as a translation
// unit: C / C++ / CUDA / OpenCL / C++ modules / assembler. Lowercase keys;
// IsCompiledSource lowercases before lookup so preprocessed-assembly `.S`
// matches `.s`.
//
// `.sx` (the case-insensitive-filesystem spelling of `.S`) is intentionally
// absent: it is not in rules_cc's ALLOWED_SRC_FILES, so Bazel never compiles a
// `.sx` source — there is no standalone-compile to classify it against.
//
// This is the single source of truth for "is this a compiled TU?", consolidating
// what lowering (textual-include routing) and split-emit (header-library
// exclusion) previously kept as two hand-maintained, drift-prone copies.
var compiledSourceExts = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".c++": true,
	".cu": true, ".cl": true, ".cppm": true, ".ixx": true,
	".s": true, ".asm": true,
}

// IsCompiledSource reports whether path's extension names a compiled translation
// unit (case-insensitively, so `.S` matches `.s`). A path with no extension —
// or one Bazel doesn't compile (headers, `.sx`, data) — returns false.
func IsCompiledSource(path string) bool {
	return compiledSourceExts[strings.ToLower(filepath.Ext(path))]
}

// headerExts are the extensions treated as C/C++ HEADERS — textually included,
// never compiled standalone (so a file with one belongs in hdrs/textual_hdrs,
// not srcs). Beyond the plain header extensions it covers the conventional
// template-implementation headers (.inl/.txx/.tcc/.ipp; e.g. VTK's
// vtkImageProgressIterator.txx) and .h++.
//
// .def and .inc cover the x-macro / textual-include idioms — a file of
// `HANDLE_FOO(...)` macro calls #included repeatedly with different macro
// definitions, or a checked-in code fragment pulled in via quote-include — that
// LLVM and many C/C++ projects use pervasively (Demangle's
// `#include "ItaniumNodes.def"`, Support's `#include "regengine.inc"`). These
// compile via their includer, so they belong in hdrs, not srcs.
//
// Single source of truth for "is this a header?", consolidating what lowering
// (route-to-hdrs classification) and build-cc-index (gazelle header indexing)
// previously kept as two divergent copies. (CUDA's .cuh stays a caller-local
// special-case in lowering — it's contextual, not a universal header ext.)
var headerExts = map[string]bool{
	".h": true, ".hh": true, ".hpp": true, ".hxx": true, ".h++": true,
	".inl": true, ".def": true, ".inc": true, ".txx": true, ".tcc": true, ".ipp": true,
}

// IsHeaderExt reports whether ext — a filename extension including the leading
// dot, already lowercased by the caller (e.g. ".hpp") — names a C/C++ header.
// For callers that already have the extension in hand; IsHeader takes a path.
func IsHeaderExt(ext string) bool {
	return headerExts[ext]
}

// IsHeader reports whether path's extension names a C/C++ header
// (case-insensitively).
func IsHeader(path string) bool {
	return headerExts[strings.ToLower(filepath.Ext(path))]
}
