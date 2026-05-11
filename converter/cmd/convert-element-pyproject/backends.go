// Per-backend package discovery for kind:pyproject.
//
// Each `discover<Backend>` function takes the parsed
// pyproject.toml + the source-root universe (a sorted slice
// of source-relative file paths) and returns a slice of
// Package structs (one per importable package directory).
// Discovery is purely declarative: we never run the
// build-backend.
//
// When a backend's discovery shape can't be statically
// resolved (e.g. setuptools without an explicit `packages`
// config), the function returns a typed
// unsupported-pyproject-package-discovery failure so the
// operator gets a clear Tier-1 signal and the converter
// falls back to the pipeline shape.
package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// Package is one importable Python package directory.
type Package struct {
	// Name is the dotted import path: "foo" for top-level,
	// "foo.bar" for sub-packages. Used as the py_library's
	// `name` (with dots → underscores for Bazel-label safety).
	Name string

	// Dir is the source-relative directory holding the
	// package's __init__.py (or, for namespace packages,
	// the namespace dir). Glob expressions in the emitted
	// py_library are anchored at this dir.
	Dir string

	// ImportRoot is the source-relative directory the package
	// is reachable FROM via Python's import path. For a flat
	// layout (`<root>/foo/__init__.py`) it's "."; for src
	// layouts (`<root>/src/foo/__init__.py`) it's "src".
	// The emitted py_library's `imports = [<this>]` attribute.
	ImportRoot string

	// Sources are source-relative .py paths in this package.
	// One py_library's srcs glob expands to (roughly) this
	// list, but we capture them explicitly so the lift can
	// emit exact srcs for layouts where glob() would over-
	// match (e.g. nested test/ subdirs we want to exclude).
	Sources []string
}

// BazelLabel is the Bazel-safe form of Package.Name (dots →
// underscores). Bazel labels accept dots in target names but
// rules_python's documentation prefers underscore-separated
// names; we follow that convention to match operator habit.
func (p Package) BazelLabel() string {
	return strings.ReplaceAll(p.Name, ".", "_")
}

// Discover dispatches on [build-system].build-backend and
// returns the per-package shape for the supported set.
// Returns a typed Tier-1 refusal for unrecognized backends.
func Discover(p *Pyproject, sourceFiles []string) ([]Package, error) {
	backend := strings.TrimSpace(p.BuildSystem.Backend)
	if backend == "" {
		return nil, newFailure(unsupportedPyprojectBackend,
			"[build-system].build-backend missing — v1 requires an explicit backend (one of: flit_core.buildapi, hatchling.build, setuptools.build_meta, poetry.core.masonry.api)")
	}
	switch backend {
	case "flit_core.buildapi":
		return discoverFlit(p, sourceFiles)
	case "hatchling.build":
		return discoverHatchling(p, sourceFiles)
	case "setuptools.build_meta":
		return discoverSetuptools(p, sourceFiles)
	case "poetry.core.masonry.api":
		return discoverPoetry(p, sourceFiles)
	default:
		return nil, newFailure(unsupportedPyprojectBackend,
			"unrecognized build-backend %q — v1 supports flit_core.buildapi / hatchling.build / setuptools.build_meta / poetry.core.masonry.api",
			backend)
	}
}

// discoverFlit handles flit_core.buildapi. flit is single-
// distribution: the wheel contains one top-level module/package
// named via `[tool.flit.module].name` or, when absent, the
// project name (with hyphens → underscores). flit's data model
// recognizes both PACKAGE shape (`<name>/__init__.py` plus
// siblings) and single-MODULE shape (just `<name>.py` at the
// import root); v1 supports both, in that order of preference.
func discoverFlit(p *Pyproject, sourceFiles []string) ([]Package, error) {
	moduleName := ""
	if p.Tool.Flit != nil && p.Tool.Flit.Module != nil {
		moduleName = p.Tool.Flit.Module.Name
	}
	if moduleName == "" {
		moduleName = strings.ReplaceAll(p.Project.Name, "-", "_")
	}
	if moduleName == "" {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"flit_core: neither [tool.flit.module].name nor [project].name set; can't infer the package name")
	}
	if pkg, ok := flitSingleModule(moduleName, sourceFiles); ok {
		return []Package{pkg}, nil
	}
	pkg, err := materializePackage(moduleName, "", sourceFiles)
	if err != nil {
		return nil, err
	}
	return []Package{pkg}, nil
}

// flitSingleModule returns a Package describing the
// `<moduleName>.py` single-file flit shape when the source
// tree contains exactly that file at the root (and no
// `<moduleName>/` package directory shadowing it). Returns
// ok=false when the shape doesn't match, leaving the caller to
// fall back to package-directory discovery.
func flitSingleModule(moduleName string, sourceFiles []string) (Package, bool) {
	target := moduleName + ".py"
	dirPrefix := moduleName + "/"
	found := false
	for _, f := range sourceFiles {
		if f == target {
			found = true
		}
		if strings.HasPrefix(f, dirPrefix) {
			// A `<moduleName>/...` directory exists alongside the
			// `<moduleName>.py` file. Flit would prefer the package
			// shape; let the caller's materializePackage path handle it.
			return Package{}, false
		}
	}
	if !found {
		return Package{}, false
	}
	return Package{
		Name:       moduleName,
		Dir:        ".",
		ImportRoot: ".",
		Sources:    []string{target},
	}, true
}

// discoverHatchling handles hatchling.build. v1 only
// recognizes the explicit-list form
// `[tool.hatch.build.targets.wheel].packages = ["src/foo"]`.
// Auto-discovery (hatch's default for VCS-tracked sources)
// would require running git here; refused.
func discoverHatchling(p *Pyproject, sourceFiles []string) ([]Package, error) {
	if p.Tool.Hatch == nil ||
		p.Tool.Hatch.Build == nil ||
		p.Tool.Hatch.Build.Targets == nil ||
		p.Tool.Hatch.Build.Targets.Wheel == nil ||
		len(p.Tool.Hatch.Build.Targets.Wheel.Packages) == 0 {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"hatchling: v1 requires [tool.hatch.build.targets.wheel].packages to be an explicit list (auto-discovery via VCS-tracked sources isn't statically resolvable)")
	}
	out := make([]Package, 0, len(p.Tool.Hatch.Build.Targets.Wheel.Packages))
	for _, dir := range p.Tool.Hatch.Build.Targets.Wheel.Packages {
		dir = strings.TrimSuffix(dir, "/")
		// hatchling's `packages` entries are source-relative
		// directory paths; the package name is the basename.
		name := filepath.Base(dir)
		root := filepath.Dir(dir)
		if root == "." {
			root = ""
		}
		pkg, err := materializePackage(name, root, sourceFiles)
		if err != nil {
			return nil, err
		}
		out = append(out, pkg)
	}
	return out, nil
}

// discoverSetuptools handles setuptools.build_meta. Two
// recognized shapes:
//  1. `[tool.setuptools] packages = ["foo", "foo.bar"]` — explicit dotted list.
//  2. `[tool.setuptools] packages.find = {where = [...], include = [...], exclude = [...]}`.
//
// setuptools' implicit auto-discovery (no `packages` config)
// is refused — the discovery rules are version-dependent and
// surfacing the exact list reliably needs running setuptools.
func discoverSetuptools(p *Pyproject, sourceFiles []string) ([]Package, error) {
	if p.Tool.Setuptools == nil || p.Tool.Setuptools.Packages == nil {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"setuptools: v1 requires either [tool.setuptools].packages = [...] or [tool.setuptools.packages.find] (setuptools' implicit auto-discovery isn't statically resolvable)")
	}
	if explicit := p.Tool.Setuptools.ExplicitPackages(); len(explicit) > 0 {
		return setuptoolsExplicit(explicit, p.Tool.Setuptools.PackageDir, sourceFiles)
	}
	find, err := p.Tool.Setuptools.FindDirective()
	if err != nil {
		// Route TOML-shape mismatches through Tier-1 so the
		// orchestrator treats the element as a typed refusal
		// (and so a future write-a per-element fallback —
		// ROADMAP Phase B option A — can dispatch it to the
		// pipeline shape) instead of aborting with a Tier-2
		// exit. The original error message is preserved
		// verbatim so the operator sees what go-toml actually
		// objected to.
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"setuptools: decode [tool.setuptools.packages.find]: %v", err)
	}
	if find == nil {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"setuptools: [tool.setuptools].packages must be a list of dotted package names or a {find = {...}} table")
	}
	return setuptoolsFind(find, sourceFiles)
}

// setuptoolsExplicit lowers the literal list form. Each entry
// is a dotted package name; we map it to a directory by
// converting dots to slashes and then check the source tree
// for the package's __init__.py (or namespace dir).
//
// `package-dir` overrides the source root — `{"" = "src"}`
// remaps every package to live under src/. v1 supports this
// common shape and refuses arbitrary per-package remaps
// (a per-package `{"foo" = "lib/foo"}` reroute) since they
// can produce non-trivial mappings we'd rather not silently
// guess at.
func setuptoolsExplicit(packages []string, packageDir map[string]string, sourceFiles []string) ([]Package, error) {
	root := ""
	for k, v := range packageDir {
		switch k {
		case "":
			root = strings.TrimSuffix(v, "/")
		default:
			return nil, newFailure(unsupportedPyprojectPackageDiscovery,
				"setuptools: per-package package-dir override (%q → %q) isn't supported in v1; only the root remap [tool.setuptools.package-dir.\"\" = \"src\"] form is recognized",
				k, v)
		}
	}
	out := make([]Package, 0, len(packages))
	for _, dotted := range packages {
		pkg, err := materializePackage(dotted, root, sourceFiles)
		if err != nil {
			return nil, err
		}
		out = append(out, pkg)
	}
	return out, nil
}

// setuptoolsFind walks the source tree for directories that
// contain an __init__.py and treats each as a package, filtered
// by `include` / `exclude` glob lists. The result is the
// universe of importable packages setuptools would have shipped
// in the wheel.
//
// v1 doesn't implement setuptools' `namespaces=true` mode (PEP
// 420 implicit namespace packages — directories without an
// __init__.py that nevertheless participate in the import
// graph). The discovery logic for those needs to walk
// subdirectories looking for any python content and emit a
// py_library per namespace component, which is structurally
// different from the __init__.py-anchored walk below. Refuse
// the namespaces=true shape with a typed Tier-1 error so the
// operator can opt the element into the pipeline-shape
// fallback rather than silently shipping an under-narrow
// package list.
func setuptoolsFind(fd *SetuptoolsFindDirective, sourceFiles []string) ([]Package, error) {
	if fd.Namespaces != nil && *fd.Namespaces {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"setuptools: [tool.setuptools.packages.find].namespaces = true (PEP 420 namespace-package discovery) isn't supported in v1; use the pipeline-shape fallback or list packages explicitly via [tool.setuptools].packages = [...]")
	}
	if len(fd.Where) > 1 {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"setuptools: [tool.setuptools.packages.find] with multiple `where` entries (%v) isn't supported in v1", fd.Where)
	}
	root := strings.TrimSuffix(fd.Where[0], "/")
	if root == "." {
		root = ""
	}
	// Every dir under root that contains an __init__.py is
	// a package. Build that set, then filter by include /
	// exclude. The dotted-name form is path/with/slashes →
	// "path.with.slashes".
	pkgs := map[string]bool{}
	prefix := root + "/"
	if root == "" {
		prefix = ""
	}
	for _, f := range sourceFiles {
		if !strings.HasPrefix(f, prefix) {
			continue
		}
		base := filepath.Base(f)
		if base != "__init__.py" {
			continue
		}
		dir := filepath.Dir(f)
		// Strip the find-where prefix to get a package-
		// relative dir, then dots-to-slashes for the dotted
		// name.
		rel := dir
		if root != "" {
			rel = strings.TrimPrefix(dir, root+"/")
		}
		dotted := strings.ReplaceAll(rel, "/", ".")
		pkgs[dotted] = true
	}
	matched := []string{}
	for dotted := range pkgs {
		if !setuptoolsIncluded(dotted, fd.Include, fd.Exclude) {
			continue
		}
		matched = append(matched, dotted)
	}
	sort.Strings(matched)
	if len(matched) == 0 {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"setuptools.packages.find: no packages matched (where=%v include=%v exclude=%v)",
			fd.Where, fd.Include, fd.Exclude)
	}
	out := make([]Package, 0, len(matched))
	for _, dotted := range matched {
		pkg, err := materializePackage(dotted, root, sourceFiles)
		if err != nil {
			return nil, err
		}
		out = append(out, pkg)
	}
	return out, nil
}

// setuptoolsIncluded matches a dotted package name against
// setuptools' include/exclude glob lists. Both lists default
// to "match anything" when empty (include) or "exclude
// nothing" (exclude). Globs use Python's fnmatch semantics
// — closely aligned with filepath.Match for our `*` /
// `[abc]` cases. We don't currently support `**` (it's not
// part of fnmatch syntax setuptools uses).
func setuptoolsIncluded(dotted string, include, exclude []string) bool {
	if len(include) > 0 {
		matched := false
		for _, pat := range include {
			if ok, _ := filepath.Match(pat, dotted); ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, pat := range exclude {
		if ok, _ := filepath.Match(pat, dotted); ok {
			return false
		}
	}
	return true
}

// discoverPoetry handles poetry.core.masonry.api. Each
// `[tool.poetry.packages]` entry has an `include` (the
// package name) and optional `from` (the source root,
// poetry's `from` mirrors setuptools' `package-dir`).
func discoverPoetry(p *Pyproject, sourceFiles []string) ([]Package, error) {
	if p.Tool.Poetry == nil || len(p.Tool.Poetry.Packages) == 0 {
		return nil, newFailure(unsupportedPyprojectPackageDiscovery,
			"poetry-core: v1 requires an explicit [tool.poetry].packages list (poetry's auto-discovery isn't statically resolvable)")
	}
	out := make([]Package, 0, len(p.Tool.Poetry.Packages))
	for _, pp := range p.Tool.Poetry.Packages {
		root := strings.TrimSuffix(pp.From, "/")
		pkg, err := materializePackage(pp.Include, root, sourceFiles)
		if err != nil {
			return nil, err
		}
		out = append(out, pkg)
	}
	return out, nil
}

// materializePackage turns a (dotted-name, importRoot) pair
// into a fully-populated Package by walking the source files
// for matching .py paths. Refuses with a typed failure when
// the dotted name doesn't correspond to any directory in the
// source tree (the package config disagrees with the source
// layout — usually a sign of a typo or a missing
// `package-dir` remap).
func materializePackage(dotted, root string, sourceFiles []string) (Package, error) {
	dir := strings.ReplaceAll(dotted, ".", "/")
	if root != "" {
		dir = root + "/" + dir
	}
	prefix := dir + "/"
	var srcs []string
	for _, f := range sourceFiles {
		if strings.HasPrefix(f, prefix) && strings.HasSuffix(f, ".py") {
			srcs = append(srcs, f)
		}
	}
	if len(srcs) == 0 {
		return Package{}, newFailure(unsupportedPyprojectPackageDiscovery,
			"package %q (dir %q): no .py files found in source tree — pyproject.toml's package config disagrees with the source layout",
			dotted, dir)
	}
	importRoot := root
	if importRoot == "" {
		importRoot = "."
	}
	return Package{
		Name:       dotted,
		Dir:        dir,
		ImportRoot: importRoot,
		Sources:    srcs,
	}, nil
}
