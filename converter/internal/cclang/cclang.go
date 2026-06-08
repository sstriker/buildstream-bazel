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
