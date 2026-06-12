package cmakecfg_test

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_Aliases re-publishes add_library(<alias> ALIAS <target>)
// redirects in the bundle so a consumer linking the alias name
// resolves it. Dangling aliases (underlying not exported) are dropped.
func TestEmit_Aliases(t *testing.T) {
	pkg := &ir.Package{
		Name:    "greetpkg",
		Targets: []ir.Target{{Name: "greeter", Kind: ir.KindCCLibrary, ArtifactName: "libgreeter.a"}},
	}
	bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{
		Namespace:   "Greeter::",
		PackageName: "Greeter",
		Aliases: []cmakecfg.Alias{
			{Name: "Greeter::Greeter", Underlying: "greeter"}, // valid
			{Name: "Greeter::Ghost", Underlying: "missing"},   // dangling — dropped
		},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	tgts := string(bundle.Files["GreeterTargets.cmake"])
	if !strings.Contains(tgts, "add_library(Greeter::Greeter ALIAS Greeter::greeter)") {
		t.Errorf("missing alias line for Greeter::Greeter:\n%s", tgts)
	}
	if strings.Contains(tgts, "Greeter::Ghost") {
		t.Errorf("dangling alias Greeter::Ghost should have been dropped:\n%s", tgts)
	}
}

var update = flag.Bool("update", false, "overwrite *.golden files")

func TestEmit_HelloWorld_Bundle(t *testing.T) {
	src, err := filepath.Abs("../../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Stable iteration over the file list for golden compare.
	var names []string
	for n := range bundle.Files {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		got := bundle.Files[name]
		goldenPath := filepath.Join("..", "..", "..", "testdata", "golden", "hello-world", "cmake-config", name+".golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated %s", goldenPath)
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("read %s (run with -update?): %v", goldenPath, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
		}
	}
}

// TestEmit_InstalledExecutable: an INSTALLED executable publishes as an
// IMPORTED executable (the protobuf::protoc shape — install(TARGETS
// <exe> EXPORT) sets ship tools downstreams drive via
// $<TARGET_FILE:Pkg::tool>): add_executable(... IMPORTED), per-config
// IMPORTED_LOCATION at the bin/ install path (no link-interface
// languages — executables have none), and the EXISTS-check entry so the
// synth prefix stubs the path. A NON-installed executable stays
// filtered (it never lands in the prefix tree). Aliases to executables
// render via add_executable(... ALIAS ...).
func TestEmit_InstalledExecutable(t *testing.T) {
	pkg := &ir.Package{
		Name: "toolpkg",
		Targets: []ir.Target{
			{Name: "gen", Kind: ir.KindCCBinary, ArtifactName: "gen", InstallDest: "bin"},
			{Name: "scratch", Kind: ir.KindCCBinary, ArtifactName: "scratch"}, // not installed
			{Name: "core", Kind: ir.KindCCLibrary, ArtifactName: "libcore.a", LinkLanguage: "C"},
		},
	}
	bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{
		Namespace:   "Tool::",
		PackageName: "Tool",
		Aliases:     []cmakecfg.Alias{{Name: "Tool::Gen", Underlying: "gen"}},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	tgts := string(bundle.Files["ToolTargets.cmake"])
	if !strings.Contains(tgts, "add_executable(Tool::gen IMPORTED)") {
		t.Errorf("installed executable not published as IMPORTED executable:\n%s", tgts)
	}
	if !strings.Contains(tgts, "add_executable(Tool::Gen ALIAS Tool::gen)") {
		t.Errorf("alias to executable must render via add_executable:\n%s", tgts)
	}
	if strings.Contains(tgts, "scratch") {
		t.Errorf("non-installed executable must stay filtered:\n%s", tgts)
	}
	rel := string(bundle.Files["ToolTargets-release.cmake"])
	if !strings.Contains(rel, `IMPORTED_LOCATION_RELEASE "${_IMPORT_PREFIX}/bin/gen"`) {
		t.Errorf("executable IMPORTED_LOCATION missing the bin/ install path:\n%s", rel)
	}
	if strings.Contains(rel, "IMPORTED_LINK_INTERFACE_LANGUAGES_RELEASE \"\"") ||
		regexpMustFind(rel, `Tool::gen PROPERTIES\n  IMPORTED_LINK`) {
		t.Errorf("executables must not carry link-interface languages:\n%s", rel)
	}
	if !strings.Contains(rel, `_cmake_import_check_files_for_Tool::gen "${_IMPORT_PREFIX}/bin/gen"`) {
		t.Errorf("EXISTS-check entry missing for the executable (synth prefix needs the stub):\n%s", rel)
	}
}

func regexpMustFind(s, pattern string) bool {
	ok, err := regexp.MatchString(pattern, s)
	return err == nil && ok
}
