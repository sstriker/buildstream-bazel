package exportshape_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/exportshape"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestEmitDeclarative_StaticLibraryArtifact(t *testing.T) {
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{
			Type:        "export",
			Destination: "lib/cmake/MyPkg",
			ExportName:  "MyPkgTargets",
			ExportTargets: []fileapi.ExportTarget{
				{Id: "foo::@", Name: "foo"},
			},
		},
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name:       "foo",
				Type:       "STATIC_LIBRARY",
				NameOnDisk: "libfoo.a",
				Install: &fileapi.TargetInstall{
					Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
				},
			},
		},
		InstallFiles: []string{
			"include/foo.h",
			"lib/libfoo.a",
			"lib/cmake/MyPkg/MyPkgTargets.cmake",
		},
	}
	out := exportshape.EmitDeclarative(in)
	if len(out) != 1 {
		t.Fatalf("want 1 target; got %d (%v)", len(out), out)
	}
	tgt := out[0]
	if tgt.Kind != ir.KindCCImport {
		t.Errorf("Kind: %v", tgt.Kind)
	}
	if tgt.Name != "foo" {
		t.Errorf("Name: %q", tgt.Name)
	}
	if tgt.StaticLibrary != "lib/libfoo.a" {
		t.Errorf("StaticLibrary: %q", tgt.StaticLibrary)
	}
	if tgt.SharedLibrary != "" {
		t.Errorf("SharedLibrary should be empty: %q", tgt.SharedLibrary)
	}
}

func TestEmitDeclarative_SharedLibraryArtifact(t *testing.T) {
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{
			Type:          "export",
			Destination:   "lib/cmake/MyPkg",
			ExportName:    "MyPkgTargets",
			ExportTargets: []fileapi.ExportTarget{{Id: "bar::@", Name: "bar"}},
		},
		Targets: map[string]fileapi.Target{
			"bar::@": {
				Name:       "bar",
				Type:       "SHARED_LIBRARY",
				NameOnDisk: "libbar.so.1",
				Install: &fileapi.TargetInstall{
					Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
				},
			},
		},
		InstallFiles: []string{"lib/libbar.so.1"},
	}
	out := exportshape.EmitDeclarative(in)
	if len(out) != 1 || out[0].SharedLibrary != "lib/libbar.so.1" {
		t.Errorf("expected SharedLibrary lib/libbar.so.1; got %+v", out)
	}
	if out[0].StaticLibrary != "" {
		t.Errorf("StaticLibrary should be empty: %q", out[0].StaticLibrary)
	}
}

func TestEmitDeclarative_InterfaceLibrary(t *testing.T) {
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{
			Type:          "export",
			Destination:   "lib/cmake/MyPkg",
			ExportName:    "MyPkgTargets",
			ExportTargets: []fileapi.ExportTarget{{Id: "iface::@", Name: "iface"}},
		},
		Targets: map[string]fileapi.Target{
			"iface::@": {Name: "iface", Type: "INTERFACE_LIBRARY"},
		},
	}
	out := exportshape.EmitDeclarative(in)
	if len(out) != 1 {
		t.Fatalf("want 1 target; got %d", len(out))
	}
	if out[0].Kind != ir.KindCCInterface {
		t.Errorf("Kind: %v; want KindCCInterface", out[0].Kind)
	}
}

func TestEmitDeclarative_HeadersFilegroup(t *testing.T) {
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{
			Type:          "export",
			Destination:   "lib/cmake/MyPkg",
			ExportName:    "MyPkgTargets",
			ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name:       "foo",
				Type:       "STATIC_LIBRARY",
				NameOnDisk: "libfoo.a",
				Install: &fileapi.TargetInstall{
					Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
				},
			},
		},
		InstallFiles: []string{"lib/libfoo.a"},
		PublicHeaders: map[string][]string{
			"foo": {"include/foo/api.h", "include/foo/util.h"},
		},
	}
	out := exportshape.EmitDeclarative(in)
	// cc_import + headers filegroup. Sorted by name: foo, foo_hdrs.
	if len(out) != 2 {
		t.Fatalf("want 2 targets; got %d (%v)", len(out), out)
	}
	if out[0].Name != "foo" || out[1].Name != "foo_hdrs" {
		t.Errorf("name order: %q / %q", out[0].Name, out[1].Name)
	}
	if out[1].Kind != ir.KindFilegroup {
		t.Errorf("hdrs target kind: %v", out[1].Kind)
	}
	if len(out[1].Srcs) != 2 {
		t.Errorf("hdrs srcs: %v", out[1].Srcs)
	}
}

func TestEmitDeclarative_BundleFilegroup(t *testing.T) {
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{
			Type:          "export",
			Destination:   "lib/cmake/MyPkg",
			ExportName:    "MyPkgTargets",
			ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name:       "foo",
				Type:       "STATIC_LIBRARY",
				NameOnDisk: "libfoo.a",
				Install: &fileapi.TargetInstall{
					Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
				},
			},
		},
		InstallFiles: []string{"lib/libfoo.a"},
		CMakeConfigBundleFiles: []string{
			"lib/cmake/MyPkg/MyPkgConfig.cmake",
			"lib/cmake/MyPkg/MyPkgConfigVersion.cmake",
			"lib/cmake/MyPkg/MyPkgTargets.cmake",
		},
		EmitConfig: true, // opt in to the config-mode bundle generation
	}
	out := exportshape.EmitDeclarative(in)
	// cc_import + bundle filegroup + one write_file producer per bundle file (3).
	if len(out) != 5 {
		t.Fatalf("want 5 targets; got %d", len(out))
	}
	// Sorted alphabetically: "cmake_config_bundle" < "foo".
	if out[0].Name != "cmake_config_bundle" {
		t.Errorf("first target should be the bundle; got %q", out[0].Name)
	}
	if out[0].Kind != ir.KindFilegroup {
		t.Errorf("bundle kind: %v", out[0].Kind)
	}
	if len(out[0].Srcs) != 3 {
		t.Errorf("bundle srcs len: %d", len(out[0].Srcs))
	}
	// Each bundle file is generated per its ROLE — Targets gets the imported
	// targets, Config include()s the Targets script, ConfigVersion gets a
	// version stub (NOT imported-target defs, which would break find_package).
	body := func(name string) string {
		for i := range out {
			if out[i].Name == name {
				return strings.Join(out[i].WriteFileContent, "\n")
			}
		}
		t.Fatalf("write_file %q not found in %v", name, out)
		return ""
	}
	targets := body("gen_lib_cmake_MyPkg_MyPkgTargets_cmake")
	if !strings.Contains(targets, "add_library(MyPkg::foo STATIC IMPORTED)") ||
		!strings.Contains(targets, "IMPORTED_LOCATION_NOCONFIG") {
		t.Errorf("Targets.cmake should carry imported-target defs; got:\n%s", targets)
	}
	cfg := body("gen_lib_cmake_MyPkg_MyPkgConfig_cmake")
	// Config.cmake glob-include()s every sibling target script (excluding the
	// Config / ConfigVersion files) so multi-export packages resolve fully.
	if !strings.Contains(cfg, `file(GLOB _bsb_target_scripts "${CMAKE_CURRENT_LIST_DIR}/*.cmake")`) ||
		!strings.Contains(cfg, `include("${_bsb_script}")`) {
		t.Errorf("Config.cmake should glob-include the sibling target scripts; got:\n%s", cfg)
	}
	if !strings.Contains(cfg, `NOT _bsb_name STREQUAL "MyPkgConfig.cmake"`) ||
		!strings.Contains(cfg, `NOT _bsb_name STREQUAL "MyPkgConfigVersion.cmake"`) {
		t.Errorf("Config.cmake must exclude the Config/ConfigVersion files from the include glob; got:\n%s", cfg)
	}
	if strings.Contains(cfg, "add_library(") {
		t.Errorf("Config.cmake must NOT carry imported-target defs; got:\n%s", cfg)
	}
	ver := body("gen_lib_cmake_MyPkg_MyPkgConfigVersion_cmake")
	if !strings.Contains(ver, "PACKAGE_VERSION_COMPATIBLE") || strings.Contains(ver, "add_library(") {
		t.Errorf("ConfigVersion.cmake should be a version stub, not targets; got:\n%s", ver)
	}
}

func TestEmitDeclarative_NamespaceFromShareLayout_AndModule(t *testing.T) {
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{
			Type:          "export",
			Destination:   "share/MyPkg/cmake", // the share/<Pkg>/cmake layout
			ExportName:    "MyPkgTargets",
			ExportTargets: []fileapi.ExportTarget{{Id: "mod::@", Name: "mod"}},
		},
		Targets: map[string]fileapi.Target{
			"mod::@": {
				Name: "mod", Type: "MODULE_LIBRARY", NameOnDisk: "libmod.so",
				Install: &fileapi.TargetInstall{Destinations: []fileapi.TargetInstallDest{{Path: "lib"}}},
			},
		},
		InstallFiles:           []string{"lib/libmod.so", "share/MyPkg/cmake/MyPkgTargets.cmake"},
		CMakeConfigBundleFiles: []string{"share/MyPkg/cmake/MyPkgTargets.cmake"},
		EmitConfig:             true,
	}
	var body string
	for _, tgt := range exportshape.EmitDeclarative(in) {
		if tgt.Kind == ir.KindWriteFile {
			body = strings.Join(tgt.WriteFileContent, "\n")
		}
	}
	// Namespace is derived as MyPkg even for share/<Pkg>/cmake (NOT "cmake"), and
	// a MODULE_LIBRARY renders as MODULE IMPORTED (not flattened to SHARED).
	if !strings.Contains(body, "add_library(MyPkg::mod MODULE IMPORTED)") {
		t.Errorf("want add_library(MyPkg::mod MODULE IMPORTED); got:\n%s", body)
	}
	if strings.Contains(body, "cmake::") {
		t.Errorf("namespace should be MyPkg, not cmake (share/<Pkg>/cmake layout):\n%s", body)
	}
}

func TestEmitDeclarative_SkipsUnresolvableArtifact(t *testing.T) {
	// libfoo.a not in InstallFiles — the artifact-resolve fails
	// silently and the cc_import is skipped (no emit). Matches
	// the "install failed but produced an empty tree" defensive
	// shape.
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{
			Type:          "export",
			Destination:   "lib/cmake/MyPkg",
			ExportName:    "MyPkgTargets",
			ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name:       "foo",
				Type:       "STATIC_LIBRARY",
				NameOnDisk: "libfoo.a",
				Install: &fileapi.TargetInstall{
					Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
				},
			},
		},
		InstallFiles: []string{"include/foo.h"}, // no libfoo.a
	}
	out := exportshape.EmitDeclarative(in)
	if len(out) != 0 {
		t.Errorf("expected no targets when artifact unresolvable; got %v", out)
	}
}

func TestEmitDeclarative_RejectsNonExportInstaller(t *testing.T) {
	in := exportshape.EmitInputs{
		Installer: fileapi.DirectoryInstaller{Type: "file"},
	}
	if got := exportshape.EmitDeclarative(in); got != nil {
		t.Errorf("non-export installer should emit nothing; got %v", got)
	}
}
