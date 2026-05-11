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
