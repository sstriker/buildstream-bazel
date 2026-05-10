// Lowering pass: takes a parsed Pyproject + the discovered
// Package list and produces typed Target structs the emit
// pass renders to BUILD.bazel.out. This is the place where
// the validation rules in docs/design/pyproject-native-render.md
// are enforced (refusal codes for c-extension / dynamic
// metadata / unresolved deps).
package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/manifest"
)

// Target is one Bazel rule the emit pass renders. Discriminated
// by Kind. Mirrors the cc-shaped IR's coarse shape but py-
// specific (no Includes / Defines / Copts).
type Target struct {
	Name string
	Kind Kind

	// py_library fields.
	Srcs       []string
	Imports    []string
	Deps       []string
	Data       []string
	Visibility []string

	// py_binary fields. EntryModule + EntryFunc are emitted
	// alongside a generated entry-shim genrule when Kind ==
	// KindPyBinary.
	EntryModule string
	EntryFunc   string
	EntryDep    string // ":<package>" — the py_library this script imports
}

// Kind discriminates Target shapes.
type Kind int

const (
	KindPyLibrary Kind = iota + 1
	KindPyBinary
)

// LowerOptions tunes the lowering pass.
type LowerOptions struct {
	// SourceFiles is the source-relative file universe the
	// converter saw. Used by the c-extension scan.
	SourceFiles []string

	// Imports, when non-nil, resolves cross-element
	// dependencies (`[project.dependencies]` entries) onto
	// Bazel labels via the shared imports manifest schema.
	Imports *manifest.Resolver
}

// Lower turns the parsed Pyproject + discovered Package list
// into a slice of Targets, or returns a typed Tier-1 failure.
func Lower(p *Pyproject, pkgs []Package, opts LowerOptions) ([]Target, error) {
	if err := refuseDynamicMetadata(p); err != nil {
		return nil, err
	}
	if err := refuseCExtensions(opts.SourceFiles); err != nil {
		return nil, err
	}

	depLabels, err := resolveDeps(p, pkgs, opts.Imports)
	if err != nil {
		return nil, err
	}

	// Disambiguate any py_library whose Bazel-label collides
	// with a [project.scripts] entry's name. Bazel target
	// names must be unique within a package, and a single-
	// module CLI (project "greet" with a console script also
	// named "greet") would otherwise produce
	// `py_library(name = "greet")` and `py_binary(name =
	// "greet")` in the same BUILD. Rename the library to
	// "<name>_lib" in that case; leave the binary using the
	// operator-facing script name (`bazel run :<script>`).
	// The renamed label flows through to the binary's
	// EntryDep below.
	scriptNames := map[string]bool{}
	for n := range p.Project.Scripts {
		scriptNames[n] = true
	}
	labelByPkgName := map[string]string{}
	for _, pk := range pkgs {
		base := pk.BazelLabel()
		if scriptNames[base] {
			base += "_lib"
		}
		labelByPkgName[pk.Name] = base
	}

	out := make([]Target, 0, len(pkgs)+len(p.Project.Scripts))
	for _, pk := range pkgs {
		out = append(out, Target{
			Name:       labelByPkgName[pk.Name],
			Kind:       KindPyLibrary,
			Srcs:       append([]string(nil), pk.Sources...),
			Imports:    []string{pk.ImportRoot},
			Deps:       append([]string(nil), depLabels...),
			Visibility: []string{"//visibility:public"},
		})
	}

	scripts, err := lowerScripts(p, pkgs, labelByPkgName)
	if err != nil {
		return nil, err
	}
	out = append(out, scripts...)
	return out, nil
}

// refuseDynamicMetadata rejects pyproject.toml shapes that
// declare load-bearing fields as `dynamic`. Build backends
// resolve `dynamic` fields at build time (e.g. setuptools_scm
// reading `git describe` for `version`); the converter doesn't
// run the backend, so we can't see the resolved value.
//
// Doc-only dynamic fields like `readme` / `description` are
// allowed — they don't affect the rendered build graph.
func refuseDynamicMetadata(p *Pyproject) error {
	loadBearing := map[string]bool{
		"version":      true,
		"dependencies": true,
		"scripts":      true,
		"gui-scripts":  true,
		"entry-points": true,
	}
	for _, d := range p.Project.Dynamic {
		if loadBearing[d] {
			return newFailure(unsupportedPyprojectDynamicMetadata,
				"[project] dynamic = [..., %q, ...] requires the build-backend to resolve %q at build time (setuptools_scm / hatch-vcs / similar). v1 doesn't run the backend; pin %q to a static value in pyproject.toml.",
				d, d, d)
		}
	}
	return nil
}

// refuseCExtensions scans the source tree for indicators of
// non-pure-Python build artifacts. Operator action: rebuild
// against the pipeline shape (which runs the backend), or
// wait for the Phase B install-plan fallback.
func refuseCExtensions(srcs []string) error {
	for _, f := range srcs {
		ext := strings.ToLower(filepath.Ext(f))
		switch ext {
		case ".c", ".cc", ".cpp", ".cxx", ".pyx", ".pxd", ".pxi", ".rs":
			return newFailure(unsupportedPyprojectCExtension,
				"source tree contains %q (%q): v1 only converts pure-Python packages; C / Cython / Rust extensions need the Phase B install-plan fallback (see ROADMAP).",
				f, ext)
		}
		if filepath.Base(f) == "Cargo.toml" {
			return newFailure(unsupportedPyprojectCExtension,
				"source tree contains a Cargo.toml — Rust extensions aren't supported in v1.")
		}
	}
	return nil
}

// resolveDeps walks [project.dependencies] and translates each
// entry into a Bazel label. Resolution order:
//
//  1. Match against the package's own packages (intra-element
//     dep — flit projects sometimes self-reference; ignored).
//  2. Imports manifest LookupCMakeTarget("<dep>").
//  3. Imports manifest LookupCMakeTarget("<dep>::<dep>") —
//     write-a's convention bind.
//  4. Otherwise: unresolved-pyproject-dependency Tier-1 failure.
//
// Stripping of PEP 508 requirement spec (version specifiers,
// markers, extras) is intentional: v1 resolves on the
// distribution NAME only. The version-pinning and markers
// belong in the imports manifest's authoring layer.
func resolveDeps(p *Pyproject, pkgs []Package, imports *manifest.Resolver) ([]string, error) {
	if len(p.Project.Dependencies) == 0 {
		return nil, nil
	}
	ownPackageNames := map[string]bool{}
	for _, pk := range pkgs {
		// Distribution names are case-insensitive and
		// hyphen/underscore-equivalent (PEP 503 normalization).
		ownPackageNames[normalizeDistName(pk.Name)] = true
		// Top-level package name in case [project].name uses
		// a dotted-prefix scheme.
		ownPackageNames[normalizeDistName(strings.SplitN(pk.Name, ".", 2)[0])] = true
	}
	if p.Project.Name != "" {
		ownPackageNames[normalizeDistName(p.Project.Name)] = true
	}

	out := []string{}
	seen := map[string]bool{}
	for _, raw := range p.Project.Dependencies {
		name := stripPEP508(raw)
		if name == "" {
			continue
		}
		norm := normalizeDistName(name)
		if ownPackageNames[norm] {
			continue
		}
		if imports != nil {
			if ex := imports.LookupCMakeTarget(name); ex != nil {
				if !seen[ex.BazelLabel] {
					out = append(out, ex.BazelLabel)
					seen[ex.BazelLabel] = true
				}
				continue
			}
			if ex := imports.LookupCMakeTarget(name + "::" + name); ex != nil {
				if !seen[ex.BazelLabel] {
					out = append(out, ex.BazelLabel)
					seen[ex.BazelLabel] = true
				}
				continue
			}
		}
		return nil, newFailure(unresolvedPyprojectDependency,
			"[project.dependencies] entry %q (normalized %q) not bound by imports manifest and not in this element's own packages",
			raw, norm)
	}
	sort.Strings(out)
	return out, nil
}

// lowerScripts maps [project.scripts] entries to py_binary
// targets. Each entry's value is `module:func`; we attach a
// dep on whichever in-graph py_library re-exports `module`,
// or refuse with unresolved-pyproject-dependency when the
// module isn't ours and isn't manifest-bound.
//
// labelByPkgName maps each package's dotted name to the
// Bazel-safe label it actually emitted as (potentially with
// a "_lib" suffix when collision-avoidance kicked in).
func lowerScripts(p *Pyproject, pkgs []Package, labelByPkgName map[string]string) ([]Target, error) {
	if len(p.Project.Scripts) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(p.Project.Scripts))
	for n := range p.Project.Scripts {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]Target, 0, len(names))
	for _, scriptName := range names {
		spec := p.Project.Scripts[scriptName]
		module, fn, err := parseEntryPoint(spec)
		if err != nil {
			return nil, fmt.Errorf("[project.scripts] %q: %w", scriptName, err)
		}
		// Attach a dep on whichever package directly contains
		// the entry module. We walk the dotted prefix space:
		// "foo.cli" matches package "foo" if "foo" exists, or
		// "foo.cli" if it's a sub-package, etc.
		dep := lookupPackageDep(module, labelByPkgName)
		if dep == "" {
			return nil, newFailure(unresolvedPyprojectDependency,
				"[project.scripts] %q = %q: module %q isn't part of this element's own packages — v1 can't lift cross-element console scripts (the binary's py_library dep would dangle)",
				scriptName, spec, module)
		}
		out = append(out, Target{
			Name:        scriptName,
			Kind:        KindPyBinary,
			EntryModule: module,
			EntryFunc:   fn,
			EntryDep:    dep,
			Visibility:  []string{"//visibility:public"},
		})
	}
	return out, nil
}

// lookupPackageDep finds the longest in-graph package whose
// dotted name is a prefix of `module`, then returns the Bazel
// label that package emitted as. Returns "" when no match
// found.
func lookupPackageDep(module string, labelByPkgName map[string]string) string {
	bestName := ""
	for pkgName := range labelByPkgName {
		if module == pkgName || strings.HasPrefix(module, pkgName+".") {
			if len(pkgName) > len(bestName) {
				bestName = pkgName
			}
		}
	}
	if bestName == "" {
		return ""
	}
	return ":" + labelByPkgName[bestName]
}

// parseEntryPoint splits `module:func` per PEP 621.
func parseEntryPoint(spec string) (module, fn string, err error) {
	idx := strings.IndexByte(spec, ':')
	if idx < 0 {
		return "", "", fmt.Errorf("entry point %q has no `:` separator (expected `module:func`)", spec)
	}
	module = strings.TrimSpace(spec[:idx])
	fn = strings.TrimSpace(spec[idx+1:])
	if module == "" || fn == "" {
		return "", "", fmt.Errorf("entry point %q has empty module or func", spec)
	}
	return module, fn, nil
}

// stripPEP508 returns just the distribution name from a PEP
// 508 requirement string. "foo>=1.2,<2; python_version<'3.10'"
// → "foo". The first non-name char (whitespace, `[`, `;`,
// version operators, etc.) terminates the name.
func stripPEP508(req string) string {
	req = strings.TrimSpace(req)
	for i, r := range req {
		switch {
		case r == ' ', r == '\t':
			return req[:i]
		case r == '[': // extras: foo[bar]
			return req[:i]
		case r == ';': // markers: foo; python_version<'3.10'
			return req[:i]
		case r == '<', r == '>', r == '=', r == '~', r == '!':
			return req[:i]
		}
	}
	return req
}

// normalizeDistName implements PEP 503's normalization:
// lowercase, runs of [-_.]+ collapse to a single `-`.
func normalizeDistName(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch r {
		case '-', '_', '.':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
