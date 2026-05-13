package main

import (
	"strings"
	"testing"
)

// minimumProject returns a Pyproject populated with just the
// fields a backend dispatch needs. Tests that exercise specific
// shapes mutate the returned value before calling Lower.
func minimumProject(backend string) *Pyproject {
	return &Pyproject{
		BuildSystem: BuildSystem{Backend: backend},
		Project:     Project{Name: "demo", Version: "0.1.0"},
	}
}

func TestLower_SetuptoolsExplicitFlatLayout(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	pkgs, err := Discover(p, []string{
		"demo/__init__.py",
		"demo/util.py",
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: []string{"demo/__init__.py", "demo/util.py"}})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d targets, want 1", len(out))
	}
	if out[0].Kind != KindPyLibrary || out[0].Name != "demo" {
		t.Errorf("target[0]=%+v want demo/py_library", out[0])
	}
	if got := out[0].Imports; len(got) != 1 || got[0] != "." {
		t.Errorf("imports=%v want [.]", got)
	}
}

func TestLower_SetuptoolsSrcLayoutWithFind(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	pkgsTOML := map[string]any{"find": map[string]any{"where": []any{"src"}}}
	p.Tool.Setuptools = &Setuptools{Packages: pkgsTOML}
	srcFiles := []string{
		"src/demo/__init__.py",
		"src/demo/cli.py",
		"src/demo/sub/__init__.py",
		"src/demo/sub/util.py",
	}
	pkgs, err := Discover(p, srcFiles)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcFiles})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	// One py_library per package directory: demo + demo.sub.
	if len(out) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(out), out)
	}
	wantNames := map[string]bool{"demo": false, "demo_sub": false}
	for _, tgt := range out {
		if _, ok := wantNames[tgt.Name]; !ok {
			t.Errorf("unexpected target name %q", tgt.Name)
			continue
		}
		wantNames[tgt.Name] = true
		if got := tgt.Imports; len(got) != 1 || got[0] != "src" {
			t.Errorf("%s: imports=%v want [src]", tgt.Name, got)
		}
	}
	for n, seen := range wantNames {
		if !seen {
			t.Errorf("missing target %q", n)
		}
	}
}

func TestLower_FlitBackend(t *testing.T) {
	p := minimumProject("flit_core.buildapi")
	p.Tool.Flit = &Flit{Module: &FlitModule{Name: "demo"}}
	srcs := []string{"demo/__init__.py", "demo/main.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 1 || out[0].Name != "demo" {
		t.Fatalf("got %+v want one demo py_library", out)
	}
}

func TestLower_FlitSingleModule(t *testing.T) {
	// Flit's single-file shape: <name>.py at the source root,
	// no <name>/ package directory. The wheel ships exactly
	// the .py file; we emit a py_library that mirrors that.
	p := minimumProject("flit_core.buildapi")
	p.Project.Name = "greet"
	srcs := []string{"greet.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("got %d packages, want 1: %+v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "greet" {
		t.Errorf("Name=%q want greet", pkgs[0].Name)
	}
	if len(pkgs[0].Sources) != 1 || pkgs[0].Sources[0] != "greet.py" {
		t.Errorf("Sources=%v want [greet.py]", pkgs[0].Sources)
	}
	if pkgs[0].ImportRoot != "." {
		t.Errorf("ImportRoot=%q want .", pkgs[0].ImportRoot)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 1 || out[0].Kind != KindPyLibrary {
		t.Fatalf("got %+v want one py_library", out)
	}
}

func TestLower_HatchlingExplicitPackages(t *testing.T) {
	p := minimumProject("hatchling.build")
	p.Tool.Hatch = &Hatch{Build: &HatchBuild{Targets: &HatchTargets{Wheel: &HatchWheel{
		Packages: []string{"src/demo"},
	}}}}
	srcs := []string{"src/demo/__init__.py", "src/demo/cli.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 1 || out[0].Name != "demo" {
		t.Fatalf("got %+v want one demo py_library", out)
	}
	if out[0].Imports[0] != "src" {
		t.Errorf("imports=%v want [src]", out[0].Imports)
	}
}

func TestLower_PoetryBackend(t *testing.T) {
	p := minimumProject("poetry.core.masonry.api")
	p.Tool.Poetry = &Poetry{Packages: []PoetryPackage{
		{Include: "demo", From: "src"},
	}}
	srcs := []string{"src/demo/__init__.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 1 || out[0].Name != "demo" {
		t.Fatalf("got %+v want demo py_library", out)
	}
}

func TestLower_RefusesUnknownBackend(t *testing.T) {
	p := minimumProject("pdm.backend")
	_, err := Discover(p, []string{"demo/__init__.py"})
	if err == nil || !strings.Contains(err.Error(), "unsupported-pyproject-backend") {
		t.Fatalf("want unsupported-pyproject-backend, got %v", err)
	}
}

func TestLower_RefusesMissingBuildSystemBackend(t *testing.T) {
	p := minimumProject("")
	_, err := Discover(p, []string{"demo/__init__.py"})
	if err == nil || !strings.Contains(err.Error(), "unsupported-pyproject-backend") {
		t.Fatalf("want unsupported-pyproject-backend, got %v", err)
	}
}

func TestLower_RefusesSetuptoolsAutoDiscovery(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	// No tool.setuptools at all → setuptools default auto-discovery,
	// which v1 refuses.
	_, err := Discover(p, []string{"demo/__init__.py"})
	if err == nil || !strings.Contains(err.Error(), "unsupported-pyproject-package-discovery") {
		t.Fatalf("want unsupported-pyproject-package-discovery, got %v", err)
	}
}

func TestLower_RefusesCExtension(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{"demo/__init__.py", "demo/_speedup.c"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_, err = Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err == nil || !strings.Contains(err.Error(), "unsupported-pyproject-c-extension") {
		t.Fatalf("want unsupported-pyproject-c-extension, got %v", err)
	}
}

func TestLower_RefusesDynamicVersion(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Project.Dynamic = []string{"version"}
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	pkgs, err := Discover(p, []string{"demo/__init__.py"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_, err = Lower(p, pkgs, LowerOptions{SourceFiles: []string{"demo/__init__.py"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported-pyproject-dynamic-metadata") {
		t.Fatalf("want unsupported-pyproject-dynamic-metadata, got %v", err)
	}
}

func TestLower_AllowsDynamicReadme(t *testing.T) {
	// Doc-only dynamic fields are accepted (don't affect build graph).
	p := minimumProject("setuptools.build_meta")
	p.Project.Dynamic = []string{"readme", "description"}
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{"demo/__init__.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs}); err != nil {
		t.Fatalf("Lower: %v", err)
	}
}

func TestLower_RefusesUnresolvedDep(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Project.Dependencies = []string{"glib-2.0"}
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	pkgs, err := Discover(p, []string{"demo/__init__.py"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_, err = Lower(p, pkgs, LowerOptions{SourceFiles: []string{"demo/__init__.py"}})
	if err == nil || !strings.Contains(err.Error(), "unresolved-pyproject-dependency") {
		t.Fatalf("want unresolved-pyproject-dependency, got %v", err)
	}
}

func TestLower_ScriptCollisionRenamesLibrary(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	p.Project.Scripts = map[string]string{
		"demo": "demo.cli:main", // script-name == library-name → must disambiguate.
	}
	srcs := []string{"demo/__init__.py", "demo/cli.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d targets, want 2 (library + binary): %+v", len(out), out)
	}
	if out[0].Name != "demo_lib" || out[0].Kind != KindPyLibrary {
		t.Errorf("library=%+v want demo_lib/py_library", out[0])
	}
	if out[1].Name != "demo" || out[1].Kind != KindPyBinary {
		t.Errorf("binary=%+v want demo/py_binary", out[1])
	}
	if out[1].EntryDep != ":demo_lib" {
		t.Errorf("EntryDep=%q want :demo_lib", out[1].EntryDep)
	}
}

func TestLower_ScriptDepFollowsLongestPrefix(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo", "demo.sub"}}
	p.Project.Scripts = map[string]string{
		"sub-cli": "demo.sub.cli:main",
	}
	srcs := []string{
		"demo/__init__.py",
		"demo/sub/__init__.py",
		"demo/sub/cli.py",
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	for _, tgt := range out {
		if tgt.Kind == KindPyBinary && tgt.Name == "sub-cli" {
			if tgt.EntryDep != ":demo_sub" {
				t.Errorf("EntryDep=%q want :demo_sub (longest-prefix match)", tgt.EntryDep)
			}
			return
		}
	}
	t.Errorf("sub-cli py_binary missing from %+v", out)
}

func TestLower_GUIScriptsEmitAsPyBinary(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	p.Project.GUIScripts = map[string]string{
		"demo-gui": "demo.gui:main",
	}
	srcs := []string{"demo/__init__.py", "demo/gui.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var gotBinary *Target
	for i := range out {
		if out[i].Kind == KindPyBinary && out[i].Name == "demo-gui" {
			gotBinary = &out[i]
			break
		}
	}
	if gotBinary == nil {
		t.Fatalf("gui-script not lowered as py_binary: %+v", out)
	}
	if gotBinary.EntryDep != ":demo" {
		t.Errorf("EntryDep=%q want :demo", gotBinary.EntryDep)
	}
}

func TestLower_NestedPackagesUseDepth1WithParentDeps(t *testing.T) {
	// When pyproject.toml declares both a parent and child
	// package (the common output of setuptools' find()), each
	// py_library should own only its own depth-1 .py files and
	// the child should depend on the parent so `import demo.sub`
	// pulls in both __init__.py files at Bazel analysis time.
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo", "demo.sub"}}
	srcs := []string{
		"demo/__init__.py",
		"demo/util.py",
		"demo/sub/__init__.py",
		"demo/sub/cli.py",
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var demo, demoSub *Target
	for i := range out {
		switch out[i].Name {
		case "demo":
			demo = &out[i]
		case "demo_sub":
			demoSub = &out[i]
		}
	}
	if demo == nil || demoSub == nil {
		t.Fatalf("want both demo + demo_sub targets, got %+v", out)
	}
	// Depth-1: demo owns its own files only, NOT demo/sub/*.
	for _, s := range demo.Srcs {
		if strings.HasPrefix(s, "demo/sub/") {
			t.Errorf("demo.Srcs leaks into sub-package: %q", s)
		}
	}
	// Parent dep: demo_sub depends on :demo so import demo.sub
	// pulls in demo/__init__.py.
	foundParentDep := false
	for _, d := range demoSub.Deps {
		if d == ":demo" {
			foundParentDep = true
		}
	}
	if !foundParentDep {
		t.Errorf("demo_sub.Deps=%v missing :demo parent", demoSub.Deps)
	}
}

func TestLower_RefusesScriptsGUIScriptsCollision(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	p.Project.Scripts = map[string]string{
		"shared": "demo.cli:main",
	}
	p.Project.GUIScripts = map[string]string{
		"shared": "demo.gui:main",
	}
	srcs := []string{"demo/__init__.py", "demo/cli.py", "demo/gui.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_, err = Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err == nil {
		t.Fatal("Lower: want refusal for scripts ↔ gui-scripts name collision")
	}
	if !strings.Contains(err.Error(), "unsupported-pyproject-entry-point") {
		t.Errorf("err=%v want unsupported-pyproject-entry-point", err)
	}
}

func TestLower_RefusesPackageNameCollisionAfterBazelLabel(t *testing.T) {
	// `a.b` and `a_b` are distinct Python packages but
	// Package.BazelLabel rewrites dots to underscores, so both
	// reduce to `a_b`. Lower's final cross-rule uniqueness check
	// must surface this as a typed Tier-1 refusal so the element
	// falls back to the pipeline shape (or the operator renames),
	// not as an invalid BUILD that bazel rejects later.
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"a.b", "a_b"}}
	srcs := []string{
		"a/__init__.py",
		"a/b/__init__.py",
		"a_b/__init__.py",
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_, err = Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err == nil {
		t.Fatal("Lower: want refusal for `a.b` vs `a_b` BazelLabel collision")
	}
	if !strings.Contains(err.Error(), "unsupported-pyproject-package-discovery") {
		t.Errorf("err=%v want unsupported-pyproject-package-discovery", err)
	}
	if !strings.Contains(err.Error(), `"a_b"`) {
		t.Errorf("err=%v should name the colliding target `a_b`", err)
	}
}

func TestLower_HatchlingRefusesNestedPath(t *testing.T) {
	// Hatchling's `packages = [...]` entries are wheel-root
	// packages: the last segment is the package, everything
	// before is the import root. A path like "src/demo/sub"
	// is ambiguous (could be `sub` rooted at `src/demo`, or
	// `demo.sub` rooted at `src`); v1 refuses rather than
	// guess.
	p := minimumProject("hatchling.build")
	p.Tool.Hatch = &Hatch{Build: &HatchBuild{Targets: &HatchTargets{Wheel: &HatchWheel{
		Packages: []string{"src/demo/sub"},
	}}}}
	srcs := []string{
		"src/demo/__init__.py",
		"src/demo/sub/__init__.py",
		"src/demo/sub/cli.py",
	}
	_, err := Discover(p, srcs)
	if err == nil {
		t.Fatalf("Discover: want refusal for multi-segment prefix path")
	}
	if !strings.Contains(err.Error(), "unsupported-pyproject-package-discovery") {
		t.Errorf("err=%v want unsupported-pyproject-package-discovery", err)
	}
}

func TestLower_SetuptoolsRefusesNonStringPackagesEntry(t *testing.T) {
	// Setuptools.ExplicitPackages used to silently drop non-
	// string entries; now it routes to a typed Tier-1 refusal
	// so a malformed TOML can't lead to a partial wheel.
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo", 42}}
	_, err := Discover(p, []string{"demo/__init__.py"})
	if err == nil {
		t.Fatalf("Discover: want refusal for non-string packages entry")
	}
	if !strings.Contains(err.Error(), "unsupported-pyproject-package-discovery") {
		t.Errorf("err=%v want unsupported-pyproject-package-discovery", err)
	}
}

func TestLower_RefusesPEP420NamespacePackage(t *testing.T) {
	// A configured package whose directory exists but has no
	// __init__.py refuses (PEP 420 namespace shape) rather than
	// emitting a py_library that wouldn't be importable.
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{"demo/cli.py"} // No __init__.py.
	_, err := Discover(p, srcs)
	if err == nil {
		t.Fatal("Discover: want refusal for namespace-package (no __init__.py)")
	}
	if !strings.Contains(err.Error(), "unsupported-pyproject-package-discovery") {
		t.Errorf("err=%v want unsupported-pyproject-package-discovery", err)
	}
}

func TestLower_RefusesUnsafeScriptName(t *testing.T) {
	// PEP 621 quoted keys allow names that would muddle Bazel
	// target / output paths; refuse on anything outside
	// [A-Za-z0-9_.-].
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	p.Project.Scripts = map[string]string{
		"bad/name": "demo.cli:main",
	}
	srcs := []string{"demo/__init__.py", "demo/cli.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_, err = Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err == nil {
		t.Fatal("Lower: want refusal for unsafe script name")
	}
	if !strings.Contains(err.Error(), "unsupported-pyproject-entry-point") {
		t.Errorf("err=%v want unsupported-pyproject-entry-point", err)
	}
}

func TestLower_SetuptoolsExplicitDedupesDuplicates(t *testing.T) {
	// `packages = ["demo", "demo"]` is functionally one
	// declaration; we dedupe rather than emit two py_library
	// targets with the same name (which would produce an
	// invalid BUILD file).
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo", "demo"}}
	srcs := []string{"demo/__init__.py", "demo/cli.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(pkgs) != 1 {
		t.Errorf("got %d packages, want 1 (duplicates deduped): %+v", len(pkgs), pkgs)
	}
}

func TestLower_SetuptoolsFindSkipsRootInitPy(t *testing.T) {
	// An `__init__.py` directly at the find-where root isn't
	// a package in setuptools' model; the finder skips it
	// instead of producing a degenerate dotted name that
	// materializePackage couldn't resolve.
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{
		Packages: map[string]any{
			"find": map[string]any{
				"where": []any{"src"},
			},
		},
	}
	srcs := []string{
		"src/__init__.py", // at the find-where root, should be skipped
		"src/demo/__init__.py",
		"src/demo/cli.py",
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(pkgs) != 1 || pkgs[0].Name != "demo" {
		t.Errorf("got %+v want one package named demo (root __init__.py skipped)", pkgs)
	}
}

func TestLower_EmitsElementNameFacade(t *testing.T) {
	// Project's dist-name (`python-dateutil`) normalizes to a
	// package directory (`dateutil`) different from the .bst
	// element name (`python-dateutil`). The element-name
	// facade lets downstream consumers reach the element via
	// the convention bind `//elements/python-dateutil:python-dateutil`
	// even though the primary py_library is named `dateutil`.
	p := minimumProject("setuptools.build_meta")
	p.Project.Name = "python-dateutil"
	p.Tool.Setuptools = &Setuptools{Packages: []any{"dateutil"}}
	srcs := []string{"dateutil/__init__.py", "dateutil/parser.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{
		SourceFiles: srcs,
		ElementName: "python-dateutil",
	})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var facade *Target
	for i := range out {
		if out[i].Name == "python-dateutil" && out[i].Kind == KindPyLibrary {
			facade = &out[i]
		}
	}
	if facade == nil {
		t.Fatalf("missing element-name facade target named %q in %+v", "python-dateutil", out)
	}
	if len(facade.Srcs) != 0 {
		t.Errorf("facade.Srcs=%v want empty (deps-only facade)", facade.Srcs)
	}
	foundDateutilDep := false
	for _, d := range facade.Deps {
		if d == ":dateutil" {
			foundDateutilDep = true
		}
	}
	if !foundDateutilDep {
		t.Errorf("facade.Deps=%v missing :dateutil", facade.Deps)
	}
}

func TestLower_SkipsElementNameFacadeOnCollision(t *testing.T) {
	// When the element name matches an existing target (the
	// common case — single-package element whose dist-name
	// equals its package name), no facade is emitted and the
	// primary py_library serves as the element's stable label.
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{"demo/__init__.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{
		SourceFiles: srcs,
		ElementName: "demo",
	})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	demoTargets := 0
	for _, t := range out {
		if t.Name == "demo" {
			demoTargets++
		}
	}
	if demoTargets != 1 {
		t.Errorf("got %d targets named demo, want 1 (no duplicate facade): %+v", demoTargets, out)
	}
}

func TestLower_SetuptoolsExplicitDotPackageDir(t *testing.T) {
	// `[tool.setuptools.package-dir]."" = "."` is a legal-but-
	// redundant way to say "project root" — equivalent to
	// omitting the package-dir override. Used to refuse with
	// "no .py files found" because the `./demo/` prefix didn't
	// match `filepath.Rel`-produced source paths (which lack
	// the leading `./`). Now normalized to "" before lookup.
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{
		Packages:   []any{"demo"},
		PackageDir: map[string]string{"": "."},
	}
	srcs := []string{"demo/__init__.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(pkgs) != 1 || pkgs[0].Name != "demo" {
		t.Errorf("got %+v want one package named demo", pkgs)
	}
}

func TestLower_PoetryDotFrom(t *testing.T) {
	// `[tool.poetry.packages]` with `from = "."` is also legal-
	// but-redundant. Same normalization fix as the setuptools
	// `package-dir`."" = "."` case above.
	p := minimumProject("poetry.core.masonry.api")
	p.Tool.Poetry = &Poetry{Packages: []PoetryPackage{{Include: "demo", From: "."}}}
	srcs := []string{"demo/__init__.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(pkgs) != 1 || pkgs[0].Name != "demo" {
		t.Errorf("got %+v want one package named demo", pkgs)
	}
}

func TestStripPEP508(t *testing.T) {
	cases := map[string]string{
		"foo":                                "foo",
		"foo>=1.2":                           "foo",
		"foo[extra]":                         "foo",
		"foo; python_version<'3.10'":         "foo",
		"foo>=1.2,<2; python_version<'3.10'": "foo",
		"foo == 1.0":                         "foo",
		"foo!=2":                             "foo",
		"foo~=1":                             "foo",
		"foo(>=1.2)":                         "foo",
		"foo,bar":                            "foo",
		"foo @ https://example/foo.tar.gz":   "foo",
	}
	for in, want := range cases {
		if got := stripPEP508(in); got != want {
			t.Errorf("stripPEP508(%q)=%q want %q", in, got, want)
		}
	}
}

func TestNormalizeDistName(t *testing.T) {
	// PEP 503: lowercase, runs of [-_.]+ collapse to '-'.
	cases := map[string]string{
		"Foo":       "foo",
		"foo-bar":   "foo-bar",
		"foo_bar":   "foo-bar",
		"foo.bar":   "foo-bar",
		"FOO__bar":  "foo-bar",
		"FOO.-_BAR": "foo-bar",
	}
	for in, want := range cases {
		if got := normalizeDistName(in); got != want {
			t.Errorf("normalizeDistName(%q)=%q want %q", in, got, want)
		}
	}
}

// TestLower_TestFilesEmitPyTest covers Phase 2's py_test
// emission: a package directory containing `test_*.py` /
// `*_test.py` files produces a sibling py_test target whose
// deps point at the package's library. The library's own srcs
// must NOT include the test files — they belong to the py_test
// rule, not the runtime library.
func TestLower_TestFilesEmitPyTest(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{
		"demo/__init__.py",
		"demo/cli.py",
		"demo/test_cli.py",  // matches HasPrefix "test_"
		"demo/util_test.py", // matches HasSuffix "_test"
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d targets, want 2 (py_library + py_test); targets=%+v", len(out), out)
	}
	lib, test := out[0], out[1]
	if lib.Kind != KindPyLibrary || lib.Name != "demo" {
		t.Errorf("target[0]=%+v want demo/py_library", lib)
	}
	// Library srcs must exclude the test files.
	wantLibSrcs := []string{"demo/__init__.py", "demo/cli.py"}
	if !equalStringSlice(lib.Srcs, wantLibSrcs) {
		t.Errorf("library.Srcs=%v want %v", lib.Srcs, wantLibSrcs)
	}
	if test.Kind != KindPyTest || test.Name != "demo_test" {
		t.Errorf("target[1]=%+v want demo_test/py_test", test)
	}
	wantTestSrcs := []string{"demo/test_cli.py", "demo/util_test.py"}
	if !equalStringSlice(test.Srcs, wantTestSrcs) {
		t.Errorf("test.Srcs=%v want %v", test.Srcs, wantTestSrcs)
	}
	// Test must dep on the library.
	if len(test.Deps) == 0 || test.Deps[0] != ":demo" {
		t.Errorf("test.Deps=%v want [:demo, ...]", test.Deps)
	}
}

// TestLower_ConftestLiftEmitsSeparateLibrary covers Phase 2's
// conftest.py lift: a package directory containing conftest.py
// emits a sibling py_library(name = "<pkg>_conftest",
// testonly = True) target that the package's py_test depends
// on (when one is emitted). Mirrors rules_python gazelle's
// convention exactly.
func TestLower_ConftestLiftEmitsSeparateLibrary(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{
		"demo/__init__.py",
		"demo/cli.py",
		"demo/conftest.py",
		"demo/test_cli.py",
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d targets, want 3 (py_library + conftest + py_test); targets=%+v", len(out), out)
	}
	// Conftest target shape.
	conftest := out[1]
	if conftest.Kind != KindPyLibrary || conftest.Name != "demo_conftest" {
		t.Errorf("target[1]=%+v want demo_conftest/py_library", conftest)
	}
	if !conftest.Testonly {
		t.Errorf("conftest.Testonly=false; want true")
	}
	if len(conftest.Srcs) != 1 || conftest.Srcs[0] != "demo/conftest.py" {
		t.Errorf("conftest.Srcs=%v want [demo/conftest.py]", conftest.Srcs)
	}
	// The conftest.py file is NOT in the library's srcs.
	lib := out[0]
	for _, s := range lib.Srcs {
		if s == "demo/conftest.py" {
			t.Errorf("library.Srcs includes conftest.py: %v", lib.Srcs)
		}
	}
	// Test wires conftest as a dep.
	test := out[2]
	wantTestDeps := []string{":demo", ":demo_conftest"}
	if len(test.Deps) < 2 || test.Deps[0] != wantTestDeps[0] || test.Deps[1] != wantTestDeps[1] {
		t.Errorf("test.Deps=%v want %v (plus depLabels)", test.Deps, wantTestDeps)
	}
}

// TestLower_PyiSrcsCollected covers Phase 2's pyi_srcs
// discovery: depth-1 .pyi files in a package directory land
// on the py_library's PyiSrcs slice. Emit.go renders them as
// `pyi_srcs = [...]`.
func TestLower_PyiSrcsCollected(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{
		"demo/__init__.py",
		"demo/cli.py",
		"demo/cli.pyi",
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d targets, want 1 (py_library only); targets=%+v", len(out), out)
	}
	lib := out[0]
	if len(lib.PyiSrcs) != 1 || lib.PyiSrcs[0] != "demo/cli.pyi" {
		t.Errorf("PyiSrcs=%v want [demo/cli.pyi]", lib.PyiSrcs)
	}
	// .pyi must not enter regular srcs.
	for _, s := range lib.Srcs {
		if strings.HasSuffix(s, ".pyi") {
			t.Errorf("library.Srcs unexpectedly contains .pyi: %v", lib.Srcs)
		}
	}
}

// TestLower_StrictModeSelfInvokingScript covers Phase 5's
// canonical py_binary shape: when the entry module self-
// invokes via `if __name__ == "__main__":`, Lower populates
// Target.Main with the entry module's source-relative path
// and Target.Srcs with the single-element list. Emit then
// renders a py_binary with no shim genrule.
func TestLower_StrictModeSelfInvokingScript(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	p.Project.Scripts = map[string]string{
		"greet": "demo.cli:main",
	}
	srcs := []string{"demo/__init__.py", "demo/cli.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	readSource := func(rel string) ([]byte, error) {
		if rel == "demo/cli.py" {
			return []byte("def main():\n    print('hi')\n\nif __name__ == \"__main__\":\n    main()\n"), nil
		}
		return nil, nil
	}
	out, err := Lower(p, pkgs, LowerOptions{
		SourceFiles: srcs,
		ReadSource:  readSource,
	})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var bin *Target
	for i := range out {
		if out[i].Kind == KindPyBinary && out[i].Name == "greet" {
			bin = &out[i]
			break
		}
	}
	if bin == nil {
		t.Fatalf("greet py_binary missing: %+v", out)
	}
	if bin.Main != "demo/cli.py" {
		t.Errorf("Main=%q want demo/cli.py (self-invoke detected)", bin.Main)
	}
	if !equalStringSlice(bin.Srcs, []string{"demo/cli.py"}) {
		t.Errorf("Srcs=%v want [demo/cli.py]", bin.Srcs)
	}
	if bin.EntryDep != ":demo" {
		t.Errorf("EntryDep=%q want :demo", bin.EntryDep)
	}
}

// TestLower_ShimModeNonSelfInvokingScript covers the Phase 5
// fallback: when the entry module DOESN'T self-invoke,
// Target.Main stays empty and emit renders the legacy shim
// genrule + py_binary shape.
func TestLower_ShimModeNonSelfInvokingScript(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	p.Project.Scripts = map[string]string{
		"greet": "demo.cli:main",
	}
	srcs := []string{"demo/__init__.py", "demo/cli.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	readSource := func(rel string) ([]byte, error) {
		return []byte("def main():\n    print('hi')\n"), nil // no guard
	}
	out, err := Lower(p, pkgs, LowerOptions{
		SourceFiles: srcs,
		ReadSource:  readSource,
	})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	for _, t0 := range out {
		if t0.Kind == KindPyBinary && t0.Name == "greet" {
			if t0.Main != "" {
				t.Errorf("Main=%q want empty (entry module doesn't self-invoke)", t0.Main)
			}
			return
		}
	}
	t.Fatalf("greet py_binary missing: %+v", out)
}

// TestLower_AlwaysEmitEntryShimForcesShim covers the operator
// opt-out: --always-emit-entry-shim forces the shim path even
// when the entry module DOES self-invoke. Target.Main stays
// empty, byte-identical to pre-Phase-5 output.
func TestLower_AlwaysEmitEntryShimForcesShim(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	p.Project.Scripts = map[string]string{
		"greet": "demo.cli:main",
	}
	srcs := []string{"demo/__init__.py", "demo/cli.py"}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	readSource := func(rel string) ([]byte, error) {
		return []byte("if __name__ == \"__main__\":\n    main()\n"), nil
	}
	out, err := Lower(p, pkgs, LowerOptions{
		SourceFiles:         srcs,
		ReadSource:          readSource,
		AlwaysEmitEntryShim: true,
	})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	for _, t0 := range out {
		if t0.Kind == KindPyBinary && t0.Name == "greet" {
			if t0.Main != "" {
				t.Errorf("Main=%q want empty (AlwaysEmitEntryShim forces shim path)", t0.Main)
			}
			return
		}
	}
	t.Fatalf("greet py_binary missing: %+v", out)
}

// TestLower_PackageMainPyEmitsBinTarget covers Phase 5's
// __main__.py detection: a package directory containing
// __main__.py gets an unconditional `<pkg>_bin` py_binary
// pointing directly at the file, matching `python -m <pkg>`.
func TestLower_PackageMainPyEmitsBinTarget(t *testing.T) {
	p := minimumProject("setuptools.build_meta")
	p.Tool.Setuptools = &Setuptools{Packages: []any{"demo"}}
	srcs := []string{
		"demo/__init__.py",
		"demo/__main__.py",
		"demo/cli.py",
	}
	pkgs, err := Discover(p, srcs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !pkgs[0].HasMain {
		t.Errorf("HasMain=false want true (package contains __main__.py)")
	}
	out, err := Lower(p, pkgs, LowerOptions{SourceFiles: srcs})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var bin *Target
	for i := range out {
		if out[i].Name == "demo_bin" {
			bin = &out[i]
			break
		}
	}
	if bin == nil {
		t.Fatalf("demo_bin missing: %+v", out)
	}
	if bin.Kind != KindPyBinary {
		t.Errorf("Kind=%v want KindPyBinary", bin.Kind)
	}
	if bin.Main != "demo/__main__.py" {
		t.Errorf("Main=%q want demo/__main__.py", bin.Main)
	}
	if !equalStringSlice(bin.Srcs, []string{"demo/__main__.py"}) {
		t.Errorf("Srcs=%v want [demo/__main__.py]", bin.Srcs)
	}
	if bin.EntryDep != ":demo" {
		t.Errorf("EntryDep=%q want :demo", bin.EntryDep)
	}
}

// TestHasSelfInvoke_VariantQuotes covers both quote-style
// variants of the canonical `if __name__ == "__main__":`
// pattern. PEP 8 prefers double quotes, but single quotes
// are equally valid Python; both must trigger the strict-
// mode emission shape.
func TestHasSelfInvoke_VariantQuotes(t *testing.T) {
	cases := map[string]bool{
		`if __name__ == "__main__":`:      true,
		`if __name__ == '__main__':`:      true,
		`if "__main__" == __name__:`:      true,
		`if __name__   ==    "__main__":`: true,
		`# if __name__ == "__main__":`:    false, // commented out
		`def f():\n    pass`:              false, // no guard
		`if name == "__main__":`:          false, // wrong identifier
	}
	for src, want := range cases {
		got := hasSelfInvoke([]byte(src))
		if got != want {
			t.Errorf("hasSelfInvoke(%q) = %v, want %v", src, got, want)
		}
	}
}

// TestEntryModuleSourcePath covers the entry-point dotted-
// name to source-relative .py path resolver. Crucial for the
// self-invoke detector to know which file to scan.
func TestEntryModuleSourcePath(t *testing.T) {
	pkgs := []Package{
		{Name: "demo", Sources: []string{"src/demo/__init__.py", "src/demo/cli.py"}},
		{Name: "demo.sub", Sources: []string{"src/demo/sub/__init__.py", "src/demo/sub/runner.py"}},
	}
	cases := []struct {
		module  string
		wantOk  bool
		wantSrc string
	}{
		{"demo.cli", true, "src/demo/cli.py"},
		{"demo.sub.runner", true, "src/demo/sub/runner.py"},
		{"demo.sub", false, ""},    // targets __init__.py — shim path
		{"unknown.mod", false, ""}, // not in graph
	}
	for _, c := range cases {
		got, ok := entryModuleSourcePath(c.module, pkgs)
		if ok != c.wantOk || got != c.wantSrc {
			t.Errorf("entryModuleSourcePath(%q) = (%q, %v); want (%q, %v)",
				c.module, got, ok, c.wantSrc, c.wantOk)
		}
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
