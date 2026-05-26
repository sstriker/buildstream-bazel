// Lowering pass: takes a parsed Pyproject + the discovered
// Package list and produces typed Target structs the emit
// pass renders to BUILD.bazel.out. This is the place where
// the validation rules in docs/architecture.md
// are enforced (refusal codes for c-extension / dynamic
// metadata / unresolved deps).
package main

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// Target is one Bazel rule the emit pass renders. Discriminated
// by Kind. Mirrors the cc-shaped IR's coarse shape but py-
// specific (no Includes / Defines / Copts).
type Target struct {
	Name string
	Kind Kind

	// py_library / py_test fields. (Package data — e.g.
	// `tool.setuptools.package-data` — is parsed but not
	// folded into a `data` attribute yet; see
	// docs/architecture.md's "What's NOT
	// covered" section for the rationale.)
	Srcs       []string
	Imports    []string
	Deps       []string
	Visibility []string

	// PyiSrcs are depth-1 .pyi stub files. Emit as
	// `pyi_srcs = [...]` on py_library / py_test when
	// non-empty. Matches rules_python gazelle plugin's
	// attribute one-for-one.
	PyiSrcs []string

	// Testonly, when true, emits `testonly = True` on the
	// rule. Used by the conftest-lift target (the
	// `<pkg>_conftest` py_library that sibling py_tests
	// depend on).
	Testonly bool

	// py_binary fields. EntryModule + EntryFunc are emitted
	// alongside a generated entry-shim genrule when Kind ==
	// KindPyBinary AND Main is empty (the shim path).
	EntryModule string
	EntryFunc   string
	EntryDep    string // ":<package>" — the py_library this script imports

	// Main, when non-empty for KindPyBinary, switches the
	// emission shape from "generate an entry-shim genrule"
	// to "point py_binary directly at a source file". Strict
	// mode (rules_python gazelle's canonical shape): the
	// emitted py_binary uses `srcs = [<Main>]` + `main =
	// <Main>` + `deps = [<EntryDep>]`, no sibling genrule.
	// Populated by Lower for two cases:
	//
	//   - A [project.scripts] entry whose target module's
	//     source file contains `if __name__ == "__main__":`
	//     and the operator hasn't opted into the
	//     back-compat shim path via --always-emit-entry-shim.
	//   - A package directory containing __main__.py — emitted
	//     unconditionally as `<pkg>_bin` per
	//     `ROADMAP.md`'s py_binary
	//     section (matches `python -m <pkg>`).
	//
	// Empty Main leaves emit.go on the shim path, byte-stable
	// with pre-Phase-5 output.
	Main string
}

// Kind discriminates Target shapes.
type Kind int

const (
	KindPyLibrary Kind = iota + 1
	KindPyBinary
	// KindPyTest renders as `py_test(...)`. Used for test
	// files detected by materializePackage (filename stem
	// `test_*` or `*_test`). Per rules_python gazelle's
	// convention: one sibling py_test per package directory
	// with test files, named "<package>_test", carrying the
	// package's library as its primary dep plus any sibling
	// conftest target.
	KindPyTest
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

	// ElementName is the .bst element name. When set and no
	// emitted py_library already has that name, Lower appends
	// a facade py_library(name = ElementName, deps = [<all
	// primary py_library labels>]) so downstream consumers can
	// reference the element via the convention bind
	// `//elements/<element-name>:<element-name>` even when the
	// primary package's py_library is named differently
	// (setuptools' dist-name → package-name normalization,
	// script-name collision suffixing _lib, etc.).
	ElementName string

	// ReadSource, when non-nil, reads a source-relative .py
	// file's bytes. lowerScripts calls it for each
	// [project.scripts] entry to detect whether the target
	// module self-invokes via `if __name__ == "__main__":`,
	// which selects the strict-mode py_binary shape (no entry-
	// shim genrule). When nil, Lower skips the self-invoke
	// check and falls back to the shim path for every script
	// — same as setting AlwaysEmitEntryShim. main.go (the
	// non-test caller) passes a closure backed by os.ReadFile
	// rooted at --source-root.
	ReadSource func(relPath string) ([]byte, error)

	// AlwaysEmitEntryShim, when true, forces the historical
	// shim-genrule path for every [project.scripts] py_binary
	// even when the target module self-invokes. Operator
	// opt-in (--always-emit-entry-shim) for projects whose
	// entry modules contain incompatible top-level side
	// effects (state mutations, sys.exit-before-main, etc.)
	// that the shim's clean `from <m> import <f>; sys.exit(f()
	// or 0)` shape avoids. Default false matches the
	// conventions doc's strict-by-default shape.
	AlwaysEmitEntryShim bool
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

	// Merge [project.scripts] and [project.gui-scripts]. Per
	// PEP 621 the two tables have identical syntax — gui-scripts
	// differs only in that it launches without a console window
	// on Windows, which is platform-runtime behavior Bazel-native
	// py_binary doesn't model. Treat them as one entry-point set
	// for emission; refuse on key collision so authors don't
	// silently shadow one with the other.
	allScripts, err := mergeScripts(p.Project.Scripts, p.Project.GUIScripts)
	if err != nil {
		return nil, err
	}

	// Disambiguate any py_library whose Bazel-label collides
	// with a [project.scripts] / [project.gui-scripts] entry's
	// name. Bazel target names must be unique within a package,
	// and a single-module CLI (project "greet" with a console
	// script also named "greet") would otherwise produce
	// `py_library(name = "greet")` and `py_binary(name =
	// "greet")` in the same BUILD. Rename the library to
	// "<name>_lib" in that case; leave the binary using the
	// operator-facing script name (`bazel run :<script>`).
	// The renamed label flows through to the binary's
	// EntryDep below.
	scriptNames := map[string]bool{}
	for n := range allScripts {
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

	out := make([]Target, 0, len(pkgs)+len(allScripts))
	for _, pk := range pkgs {
		deps := append([]string(nil), depLabels...)
		// Wire parent-package dep so `import demo.sub` pulls in
		// demo's __init__.py at Bazel analysis time. materialize-
		// Package collects .py depth-1 only, so each py_library
		// owns just its own dir; the parent dep restores the
		// transitive import semantics. Longest-prefix match
		// against in-graph packages, so for {demo, demo.sub,
		// demo.sub.cli}, demo.sub.cli → :demo_sub → :demo
		// chains correctly.
		if parent := longestInGraphParent(pk.Name, labelByPkgName); parent != "" {
			deps = append(deps, ":"+parent)
		}
		out = append(out, Target{
			Name:       labelByPkgName[pk.Name],
			Kind:       KindPyLibrary,
			Srcs:       append([]string(nil), pk.Sources...),
			PyiSrcs:    append([]string(nil), pk.PyiSources...),
			Imports:    []string{pk.ImportRoot},
			Deps:       deps,
			Visibility: []string{"//visibility:public"},
		})

		// conftest.py lift (per rules_python gazelle's
		// convention): each package directory containing a
		// conftest.py gets its own py_library(testonly = True)
		// that sibling py_tests depend on. Named
		// "<package>_conftest" — namespaced under the package
		// to avoid collisions when multiple packages in the
		// same BUILD ship conftests. Wires only the depth-1
		// conftest.py as srcs; pytest finds it at test time
		// via the conftest discovery mechanism.
		conftestLabel := ""
		if pk.HasConftest {
			conftestName := labelByPkgName[pk.Name] + "_conftest"
			conftestLabel = ":" + conftestName
			out = append(out, Target{
				Name:       conftestName,
				Kind:       KindPyLibrary,
				Srcs:       []string{pk.Dir + "/conftest.py"},
				Imports:    []string{pk.ImportRoot},
				Testonly:   true,
				Visibility: []string{"//visibility:public"},
			})
		}

		// py_test emission for the package's test files.
		// Sibling target named "<package>_test"; deps include
		// the package's library (so the test sees the runtime
		// it's exercising) and any sibling conftest.
		if len(pk.TestSources) > 0 {
			testDeps := []string{":" + labelByPkgName[pk.Name]}
			if conftestLabel != "" {
				testDeps = append(testDeps, conftestLabel)
			}
			testDeps = append(testDeps, depLabels...)
			// PyiSrcs intentionally NOT duplicated onto the
			// py_test target: the stubs ship on the package's
			// py_library, which the test deps onto — so the
			// stubs are transitively reachable to consumers of
			// the test target without us emitting them twice.
			out = append(out, Target{
				Name:       labelByPkgName[pk.Name] + "_test",
				Kind:       KindPyTest,
				Srcs:       append([]string(nil), pk.TestSources...),
				Imports:    []string{pk.ImportRoot},
				Deps:       testDeps,
				Visibility: []string{"//visibility:public"},
			})
		}
	}

	scripts, err := lowerScripts(allScripts, pkgs, labelByPkgName, opts)
	if err != nil {
		return nil, err
	}
	out = append(out, scripts...)

	// `__main__.py` package-bin emission. Per
	// `ROADMAP.md`'s py_binary
	// section: a package directory containing `__main__.py`
	// is `python -m <pkg>`-runnable; matching rules_python's
	// gazelle convention, we emit an unconditional
	// `py_binary(name = "<pkg>_bin", srcs = ["<pkg>/__main__.py"],
	// main = "<pkg>/__main__.py", deps = [":<pkg>"])`.
	// Independent of [project.scripts] — a package can ship
	// both a scripts entry-point AND a __main__.py runnable
	// form, and operators expect both to be reachable from
	// Bazel.
	for _, pk := range pkgs {
		if !pk.HasMain {
			continue
		}
		binName := labelByPkgName[pk.Name] + "_bin"
		if targetNameExists(out, binName) {
			// Collision against a user-declared script-name
			// rename or an element-name facade; skip rather
			// than producing a duplicate. The renamed library
			// + script binary already cover the entry-point.
			continue
		}
		mainPath := path.Join(pk.Dir, "__main__.py")
		out = append(out, Target{
			Name:       binName,
			Kind:       KindPyBinary,
			Main:       mainPath,
			Srcs:       []string{mainPath},
			EntryDep:   ":" + labelByPkgName[pk.Name],
			Visibility: []string{"//visibility:public"},
		})
	}

	// Element-name facade. The imports.json contract (set by
	// write-a's writePyprojectImportsManifest + the kind-agnostic
	// convention bind) is that downstream consumers reach this
	// element via `//elements/<element-name>:<element-name>`.
	// The per-package py_library names above don't necessarily
	// match — dist-name often differs from the package directory
	// (setuptools normalization), and the script-collision
	// rename can suffix the library with _lib. Emit a stable
	// facade py_library that aggregates the per-package targets
	// (no srcs of its own; deps pull each one in transitively)
	// when ElementName is set AND not already a target name.
	if opts.ElementName != "" && !targetNameExists(out, opts.ElementName) {
		facadeDeps := make([]string, 0, len(pkgs))
		for _, pk := range pkgs {
			facadeDeps = append(facadeDeps, ":"+labelByPkgName[pk.Name])
		}
		out = append(out, Target{
			Name:       opts.ElementName,
			Kind:       KindPyLibrary,
			Deps:       facadeDeps,
			Visibility: []string{"//visibility:public"},
		})
	}
	// Final uniqueness check across every emitted target name.
	// Bazel target names must be unique within a package, so two
	// targets with the same Name would produce an invalid BUILD.
	// The library/binary/facade emission paths above each handle
	// their own narrow collision case (BazelLabel's dot-to-
	// underscore rewrite for sibling packages, the script-name
	// /library-name `_lib` suffix, the element-name facade's
	// targetNameExists guard), but they don't cross-check each
	// other — a real package literally named `<x>_lib` would
	// collide with the renamed `<x>` library when `<x>` is also
	// a script name, and `a.b` + `a_b` siblings both BazelLabel
	// to `a_b`. Surface as a typed Tier-1 refusal so the
	// operator renames one of the conflicting packages (or
	// re-renders without --convert-element-pyproject to take
	// the pipeline-shape default), rather than emitting an
	// invalid BUILD that bazel rejects with a less actionable
	// error.
	if collision := firstTargetNameCollision(out); collision != "" {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"target-name collision: %q is emitted by more than one rule (typically a sibling-package shape like `a.b` vs `a_b` whose Bazel labels both reduce to `a_b`, or a script-collision _lib suffix matching a literal `<x>_lib` package). Rename one of the conflicting packages in pyproject.toml.",
			collision)
	}
	return out, nil
}

// firstTargetNameCollision returns the first target name that
// appears more than once in targets, or "" if every name is
// unique. Used by Lower's final cross-rule uniqueness check
// before emission — the per-rule emission paths handle their
// own narrow collisions, but only the combined view can catch
// cross-rule shapes (sibling-package dot/underscore equivalence,
// _lib suffix vs literal `<x>_lib` package).
func firstTargetNameCollision(targets []Target) string {
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.Name] {
			return t.Name
		}
		seen[t.Name] = true
	}
	return ""
}

// targetNameExists reports whether any target in `targets` has
// the given name. Used by the element-name facade emission to
// avoid colliding with an existing primary py_library or
// py_binary that happens to match the element name.
func targetNameExists(targets []Target, name string) bool {
	for _, t := range targets {
		if t.Name == name {
			return true
		}
	}
	return false
}

// mergeScripts unions [project.scripts] and [project.gui-scripts]
// into a single name → entry-point map. Returns a typed Tier-1
// failure when both tables declare the same name (Bazel would
// otherwise emit two py_binary rules with the same target name).
func mergeScripts(scripts, guiScripts map[string]string) (map[string]string, error) {
	if len(scripts) == 0 && len(guiScripts) == 0 {
		return nil, nil
	}
	merged := make(map[string]string, len(scripts)+len(guiScripts))
	for n, v := range scripts {
		merged[n] = v
	}
	for n, v := range guiScripts {
		if existing, ok := merged[n]; ok {
			return nil, newFailure(unsupportedPyprojectEntryPoint,
				"entry-point name %q declared in both [project.scripts] (%q) and [project.gui-scripts] (%q); v1 can't disambiguate the two into distinct py_binary rules",
				n, existing, v)
		}
		merged[n] = v
	}
	return merged, nil
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
		if path.Base(f) == "Cargo.toml" {
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
		// pk.Name carries the discovered package's fully
		// qualified dotted name; matching deps against this
		// canonical form catches the typical
		// `[project].name = "google-cloud-storage"` /
		// package = `google/cloud/storage/` case. Adding the
		// top-level component ("google") here would
		// over-approximate ownership and silently suppress a
		// legitimate external dep on a different dist that
		// happens to share that prefix (e.g. a real `google`
		// dist) — so only the fully qualified form is
		// recorded.
		ownPackageNames[normalizeDistName(pk.Name)] = true
	}
	if p.Project.Name != "" {
		ownPackageNames[normalizeDistName(p.Project.Name)] = true
	}

	out := []string{}
	seen := map[string]bool{}
	for _, raw := range p.Project.Dependencies {
		// Empty entries (`[project.dependencies] = [""]`) are
		// always-skipped no-ops, not an under-approximation
		// risk — there's nothing to resolve. But a non-empty raw
		// that stripPEP508 reduces to "" means the requirement
		// starts with a non-name char (e.g. `,foo`, `>=1.2`):
		// a malformed PEP 508 string the user almost certainly
		// didn't mean to silently drop. Surface as Tier-1 so
		// the operator fixes the typo (or re-renders without
		// --convert-element-pyproject to take the pipeline-shape
		// default), rather than building an under-approximated
		// deps graph that would import-fail at runtime.
		if strings.TrimSpace(raw) == "" {
			continue
		}
		name := stripPEP508(raw)
		if name == "" {
			return nil, newFailure(unresolvedPyprojectDependency,
				"[project.dependencies] entry %q isn't a parseable PEP 508 requirement (stripped to an empty distribution name — the requirement starts with a non-name character like `,` / `;` / `>=` / `@`, or otherwise doesn't begin with a valid identifier). Fix the entry to start with the distribution name (e.g. `requests>=2.0` not `>=2.0`).",
				raw)
		}
		norm := normalizeDistName(name)
		if ownPackageNames[norm] {
			continue
		}
		if ex := lookupDistInManifest(imports, name, norm); ex != nil {
			if !seen[ex.BazelLabel] {
				out = append(out, ex.BazelLabel)
				seen[ex.BazelLabel] = true
			}
			continue
		}
		if imports == nil {
			return nil, newFailure(unresolvedPyprojectDependency,
				"[project.dependencies] entry %q (normalized %q) isn't in this element's own packages, and no --imports-manifest was provided to look it up in. Pass --imports-manifest=<path> with a manifest entry that maps %q to a Bazel label.",
				raw, norm, norm)
		}
		return nil, newFailure(unresolvedPyprojectDependency,
			"[project.dependencies] entry %q (normalized %q) not bound by imports manifest and not in this element's own packages",
			raw, norm)
	}
	sort.Strings(out)
	return out, nil
}

// lookupDistInManifest tries the imports-manifest's
// LookupCMakeTarget against several name variants so PEP 503
// normalization (lowercase, runs of [-_.]+ → -) matches
// regardless of whether the manifest authoring layer uses the
// raw name from pyproject.toml or the normalized form. Tries,
// in order: raw, normalized, raw `::` raw, normalized `::`
// normalized. Returns nil when no variant matches or imports
// is nil.
//
// Why we try multiple shapes rather than canonicalizing the
// manifest at load time: the manifest schema is shared across
// kinds (cmake / autotools / meson), and those use namespaced
// CMake target names like `Glibc::c` where PEP 503
// normalization would be wrong. Per-kind variant lookups keeps
// the manifest's authoring contract kind-agnostic.
func lookupDistInManifest(imports *manifest.Resolver, raw, normalized string) *manifest.Export {
	if imports == nil {
		return nil
	}
	for _, variant := range []string{
		raw,
		normalized,
		raw + "::" + raw,
		normalized + "::" + normalized,
	} {
		if ex := imports.LookupCMakeTarget(variant); ex != nil {
			return ex
		}
	}
	return nil
}

// lowerScripts maps a merged scripts + gui-scripts map to
// py_binary targets. Each entry's value is `module:func`; we
// attach a dep on whichever py_library re-exports `module`:
//
//  1. First try the in-graph packages (longest dotted-prefix
//     match — most common case).
//  2. Otherwise try the imports manifest: the entry module's
//     top-level component is treated as the distribution name
//     and looked up under PEP 503 normalization. Matches the
//     resolveDeps resolution shape so cross-element scripts
//     (a console-script imported from a manifest-bound dep)
//     work the same way [project.dependencies] entries do.
//  3. Otherwise refuse with unresolved-pyproject-dependency.
//
// labelByPkgName maps each in-graph package's dotted name to
// the Bazel-safe label it actually emitted as (potentially
// with a "_lib" suffix when script-name collision-avoidance
// kicked in).
//
// pkgs is the discovered Package list; needed to resolve an
// entry-point module dotted name to a source-relative .py
// path for the Phase 5 self-invoke detection.
//
// opts carries the Phase 5 knobs: ReadSource for self-invoke
// detection and AlwaysEmitEntryShim for the operator opt-out.
func lowerScripts(scripts map[string]string, pkgs []Package, labelByPkgName map[string]string, opts LowerOptions) ([]Target, error) {
	if len(scripts) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(scripts))
	for n := range scripts {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]Target, 0, len(names))
	for _, scriptName := range names {
		spec := scripts[scriptName]
		if !isValidBazelTargetName(scriptName) {
			return nil, newFailure(unsupportedPyprojectEntryPoint,
				"entry-point name %q can't be used as a Bazel target name (only ASCII letters / digits / `_` / `-` / `.` allowed; no `/`, no path traversal, no whitespace)",
				scriptName)
		}
		module, fn, err := parseEntryPoint(spec)
		if err != nil {
			return nil, fmt.Errorf("[project.scripts] %q: %w", scriptName, err)
		}
		// Step 1: in-graph longest-prefix match.
		dep := lookupPackageDep(module, labelByPkgName)
		// Step 2: imports manifest under PEP 503 normalization
		// against the entry module's top-level component (which
		// is the distribution name in the typical
		// `dist_name.cli:main` shape).
		if dep == "" {
			topLevel := strings.SplitN(module, ".", 2)[0]
			if ex := lookupDistInManifest(opts.Imports, topLevel, normalizeDistName(topLevel)); ex != nil {
				dep = ex.BazelLabel
			}
		}
		if dep == "" {
			if opts.Imports == nil {
				return nil, newFailure(unresolvedPyprojectDependency,
					"[project.scripts] %q = %q: module %q isn't part of this element's own packages, and no --imports-manifest was provided to look it up in. Pass --imports-manifest=<path> with an entry that maps the top-level distribution name to a Bazel label.",
					scriptName, spec, module)
			}
			return nil, newFailure(unresolvedPyprojectDependency,
				"[project.scripts] %q = %q: module %q isn't part of this element's own packages and isn't bound by the imports manifest (the binary's py_library dep would dangle)",
				scriptName, spec, module)
		}
		t := Target{
			Name:        scriptName,
			Kind:        KindPyBinary,
			EntryModule: module,
			EntryFunc:   fn,
			EntryDep:    dep,
			Visibility:  []string{"//visibility:public"},
		}
		// Phase 5: strict-mode self-invoke detection. When the
		// target module's source file contains `if __name__ ==
		// "__main__":`, emit `py_binary` pointing directly at
		// the module file — no sibling shim genrule. Falls back
		// to the shim path when:
		//   - AlwaysEmitEntryShim is set (operator opt-in),
		//   - ReadSource is nil (no I/O available; e.g. probe
		//     mode or a caller that didn't wire the callback),
		//   - the entry module's source file can't be located
		//     in pkgs (cross-element scripts reaching modules
		//     outside this element's own package list),
		//   - the file's bytes can't be read,
		//   - the bytes don't contain a self-invoke pattern.
		// The shim path stays universally compatible.
		if !opts.AlwaysEmitEntryShim && opts.ReadSource != nil {
			if srcPath, ok := entryModuleSourcePath(module, pkgs); ok {
				if content, rerr := opts.ReadSource(srcPath); rerr == nil && hasSelfInvoke(content) {
					t.Main = srcPath
					t.Srcs = []string{srcPath}
				}
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// entryModuleSourcePath maps an entry-point dotted module to
// a source-relative .py file path by locating the longest
// in-graph package whose dotted name is a prefix of `module`,
// then matching the remaining tail against one of that
// package's source files.
//
// Returns ("", false) when the module doesn't resolve to a
// single file in any in-graph package — typically a cross-
// element module reached via the imports manifest, or a
// module path that targets the package's __init__.py
// directly (a self-invoke check on __init__.py is unusual;
// fall back to the shim path).
func entryModuleSourcePath(module string, pkgs []Package) (string, bool) {
	bestIdx := -1
	bestLen := -1
	for i, pk := range pkgs {
		if module == pk.Name || strings.HasPrefix(module, pk.Name+".") {
			if len(pk.Name) > bestLen {
				bestLen = len(pk.Name)
				bestIdx = i
			}
		}
	}
	if bestIdx < 0 {
		return "", false
	}
	pk := pkgs[bestIdx]
	// The remaining-tail-after-package is the leaf module name
	// (or "" when module == pk.Name, which means the entry-point
	// targets the package's __init__.py).
	tail := strings.TrimPrefix(module, pk.Name)
	tail = strings.TrimPrefix(tail, ".")
	if tail == "" {
		// Entry-point targets the package's __init__.py.
		// Less common shape; let the shim path handle it.
		return "", false
	}
	if strings.Contains(tail, ".") {
		// Sub-sub-module that lives in a child package, not
		// in this package's depth-1 source list. The longest-
		// prefix-match-loop above picked the closest package;
		// if even that one's depth-1 doesn't have the file,
		// fall back to the shim path.
		return "", false
	}
	want := tail + ".py"
	for _, s := range pk.Sources {
		if path.Base(s) == want {
			return s, true
		}
	}
	return "", false
}

// hasSelfInvoke reports whether the given Python source
// contains an `if __name__ == "__main__":` guard at module
// scope. We do a textual scan rather than a real Python AST
// parse: the import is light, the false-positive rate is
// negligible (the literal string is essentially unique in
// real .py files outside the guard pattern), and a
// false negative (caller falls back to the shim path) is
// strictly conservative — the shim path is always correct,
// it just produces an extra genrule.
//
// Accepts both quote styles (`"__main__"` and `'__main__'`)
// and tolerates whitespace variations around the operator.
// Doesn't try to detect nested-block guards (e.g. one inside
// a function body) — those aren't the runnable shape and the
// scan's conservative approximation is fine for them.
func hasSelfInvoke(src []byte) bool {
	// Walk lines; skip leading whitespace; skip comments;
	// match the canonical guard with either quote style and
	// whitespace tolerance.
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "if") {
			continue
		}
		// Collapse interior whitespace then probe both quoting
		// variants. Both shapes — `if __name__ == "__main__":`
		// and `if __name__ == '__main__':` — are PEP 8-canonical
		// and equivalent at runtime.
		collapsed := collapseSpaces(line)
		if strings.HasPrefix(collapsed, `if __name__ == "__main__"`) ||
			strings.HasPrefix(collapsed, `if __name__ == '__main__'`) ||
			strings.HasPrefix(collapsed, `if "__main__" == __name__`) ||
			strings.HasPrefix(collapsed, `if '__main__' == __name__`) {
			return true
		}
	}
	return false
}

// collapseSpaces replaces every run of ASCII whitespace
// (space or tab) with a single space. Used by hasSelfInvoke
// so `if __name__   ==    "__main__":` matches the canonical
// `if __name__ == "__main__":` form.
func collapseSpaces(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}

// longestInGraphParent finds the longest in-graph package whose
// dotted name is a strict prefix of `dotted` (matched at a `.`
// boundary), then returns the Bazel label that package emitted
// as. Returns "" when no parent is in the graph (top-level
// package). Used to wire each py_library's parent-package dep
// so `import a.b.c` pulls in a's and a.b's __init__.py files
// transitively, even though materializePackage collects sources
// at depth-1 only.
func longestInGraphParent(dotted string, labelByPkgName map[string]string) string {
	bestName := ""
	for pkgName := range labelByPkgName {
		if pkgName == dotted {
			continue
		}
		if strings.HasPrefix(dotted, pkgName+".") && len(pkgName) > len(bestName) {
			bestName = pkgName
		}
	}
	if bestName == "" {
		return ""
	}
	return labelByPkgName[bestName]
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

// parseEntryPoint splits `module:func` per PEP 621 and validates
// both parts against Python identifier syntax. The strict-subset
// validation is load-bearing: emit.go embeds module + func into
// a shell-quoted `printf` snippet inside the generated genrule's
// cmd attribute; a hostile entry like `mod'; rm -rf /; echo ':x`
// could otherwise inject shell syntax into the rendered BUILD.
// Python module names are `[A-Za-z_][A-Za-z0-9_]*` joined by `.`;
// function names are `[A-Za-z_][A-Za-z0-9_]*`. Anything else
// refuses with the typed Tier-1 unsupported-pyproject-entry-point
// code, which the orchestrator routes as a typed Tier-1 failure
// (and which a future write-a per-element fallback dispatch can
// route to the pipeline-shape fallback — queued in ROADMAP as
// Phase B option A) rather than aborting with a Tier-2 exit.
func parseEntryPoint(spec string) (module, fn string, err error) {
	idx := strings.IndexByte(spec, ':')
	if idx < 0 {
		return "", "", newFailure(unsupportedPyprojectEntryPoint,
			"entry point %q has no `:` separator (expected `module:func`)", spec)
	}
	module = strings.TrimSpace(spec[:idx])
	fn = strings.TrimSpace(spec[idx+1:])
	if module == "" || fn == "" {
		return "", "", newFailure(unsupportedPyprojectEntryPoint,
			"entry point %q has empty module or func", spec)
	}
	if !isValidDottedModule(module) {
		return "", "", newFailure(unsupportedPyprojectEntryPoint,
			"entry point %q: module %q isn't a valid dotted Python identifier (each component must match [A-Za-z_][A-Za-z0-9_]*)", spec, module)
	}
	if !isValidIdentifier(fn) {
		return "", "", newFailure(unsupportedPyprojectEntryPoint,
			"entry point %q: function %q isn't a valid Python identifier ([A-Za-z_][A-Za-z0-9_]*)", spec, fn)
	}
	return module, fn, nil
}

// isValidIdentifier reports whether s matches the Python
// identifier subset `[A-Za-z_][A-Za-z0-9_]*`. Used by
// parseEntryPoint to reject inputs that could escape the
// shell-quoting in the generated entry-shim genrule.
func isValidIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r == '_':
			continue
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
			continue
		default:
			return false
		}
	}
	return true
}

// isValidBazelTargetName reports whether s is safe to use as a
// Bazel target name. Bazel itself accepts a broader character
// set in target names (letters, digits, `_`, `-`, `.`, `+`,
// `=`, `,`, `@`, `~`, plus `/` as a literal part of the name —
// not a package separator). We're stricter: only
// `[A-Za-z0-9_.-]+`, since the target name flows into:
//   - the py_binary rule name,
//   - the sibling entry-shim genrule's `name`,
//   - the genrule's `outs = ["<name>_entry.py"]` filename,
//
// and a PEP 621 quoted-key script with `/`, whitespace, `..`,
// or shell-special characters would either invalidate the
// genrule output path or muddle the rendered BUILD shape.
// Anything outside that subset refuses with
// unsupported-pyproject-entry-point.
func isValidBazelTargetName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			continue
		default:
			return false
		}
	}
	return true
}

// isValidDottedModule reports whether s is a non-empty
// dot-separated list of valid Python identifiers (e.g.
// "foo.bar.baz").
func isValidDottedModule(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !isValidIdentifier(part) {
			return false
		}
	}
	return true
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
		case r == '(': // parenthesized version: foo(>=1.2)
			return req[:i]
		case r == ',': // chained version specs: foo>=1.2,<2
			return req[:i]
		case r == ';': // markers: foo; python_version<'3.10'
			return req[:i]
		case r == '@': // URL form: foo @ https://...
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
