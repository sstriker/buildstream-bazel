// Package toolchainscan enumerates the cc_toolchain feature vocabulary an
// operator's Bazel toolchain declares, by statically parsing the toolchain's
// Starlark with the buildtools (buildifier) BUILD/.bzl parser.
//
// It exists so the raw-flag → feature lift (lower.liftRawFeatureFlags, gated by
// toolchainfeature.RewriteFeatureWith) can target the operator's REAL toolchain
// vocabulary instead of the converter's generated default — the operator points
// the converter at their toolchain and the lift only rewrites flags onto
// features that toolchain actually defines.
//
// # Supported syntax
//
// ParseDeclared recognizes a feature NAME when it is a STRING LITERAL in a
// direct call to cc_toolchain_config_lib's feature():
//
//	feature(name = "opt")        // keyword arg (the cc_toolchain_config_lib norm)
//	feature("opt")               // first positional arg
//	feature(name = "opt", enabled = True, flag_sets = [...])
//
// A call is matched purely by its callee being an identifier literally named
// `feature` — there is NO load resolution, so any `feature(name=…)` call
// matches regardless of where (or whether) `feature` was loaded, and a
// same-named local would match too. This is the cc_toolchain_config_lib
// convention in practice; the trade-off for needing no Bazel workspace.
//
// # NOT supported (by design)
//
// Because this is a parse, not an evaluation, a feature name that only exists
// after Starlark runs is invisible:
//
//   - computed names: feature(name = "san_" + s), feature(name = mode)
//   - names produced in a loop/comprehension over a non-literal list
//   - features declared via a WRAPPER function (e.g. the converter's own
//     _feature_with_flags("asan", ...) → feature(name = name)); the literal
//     lives at the call site, not the feature() call, so it's not followed.
//
// Resolving those would need full rule evaluation (a ctx + cc_common +
// provider environment), which is deliberately out of scope. In practice
// hand-written and cc_toolchain_config_lib-style toolchains name features with
// literals, so the parse is exact for them; for anything dynamic, point the
// converter at a config that uses literal feature() names (or fall back to the
// generated default vocabulary).
package toolchainscan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bazelbuild/buildtools/build"
)

// ParseDeclared returns the sorted, de-duplicated set of feature names declared
// (as literals) by the toolchain Starlark at path. path may be a single .bzl
// file or a directory, in which case its top-level *.bzl files are parsed and
// their declared features unioned.
//
// A directory with no .bzl files, or a file declaring no feature() calls,
// returns an empty slice (not an error) — the caller decides whether an empty
// vocabulary is meaningful (it would lift only the built-in `pic`).
func ParseDeclared(path string) ([]string, error) {
	files, err := bzlFiles(path)
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for _, f := range files {
		names, err := featuresInFile(f)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			set[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// bzlFiles resolves path to the list of .bzl files to parse: the file itself
// when path is a regular file, or the top-level *.bzl entries when it's a dir.
func bzlFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("toolchainscan: %w", err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("toolchainscan: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".bzl" {
			continue
		}
		out = append(out, filepath.Join(path, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// featuresInFile parses one .bzl and returns the literal names of every
// feature() call in it (keyword `name =` or first positional string).
func featuresInFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("toolchainscan: %w", err)
	}
	f, err := build.ParseBzl(path, data)
	if err != nil {
		return nil, fmt.Errorf("toolchainscan: parse %s: %w", path, err)
	}
	var names []string
	build.Walk(f, func(expr build.Expr, _ []build.Expr) {
		call, ok := expr.(*build.CallExpr)
		if !ok {
			return
		}
		if ident, ok := call.X.(*build.Ident); !ok || ident.Name != "feature" {
			return
		}
		if n, ok := featureName(call); ok {
			names = append(names, n)
		}
	})
	return names, nil
}

// featureName extracts the literal feature name from a feature() call:
// the `name = "..."` keyword argument, else the first positional string.
func featureName(call *build.CallExpr) (string, bool) {
	for _, arg := range call.List {
		if as, ok := arg.(*build.AssignExpr); ok {
			if lhs, ok := as.LHS.(*build.Ident); ok && lhs.Name == "name" {
				if s, ok := as.RHS.(*build.StringExpr); ok {
					return s.Value, true
				}
				return "", false // name = <non-literal>: can't resolve
			}
		}
	}
	// No keyword name=; a positional name is only valid as the FIRST argument
	// (Starlark forbids a positional after a keyword), so accept the first arg
	// when it's a string literal and ignore everything else.
	if len(call.List) > 0 {
		if s, ok := call.List[0].(*build.StringExpr); ok {
			return s.Value, true
		}
	}
	return "", false
}
