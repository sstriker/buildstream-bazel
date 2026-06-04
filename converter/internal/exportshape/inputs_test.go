package exportshape_test

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/exportshape"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestBuildInputs_StaticLibraryArtifactSynthesized(t *testing.T) {
	// One STATIC_LIBRARY target with install destination "lib"
	// and NameOnDisk "libfoo.a" → InstallFiles carries
	// "lib/libfoo.a"; the bundle script lands at
	// "lib/cmake/MyPkg/MyPkgTargets.cmake".
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/MyPkg",
		ExportName:    "MyPkgTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
	}
	targets := map[string]fileapi.Target{
		"foo::@": {
			Name:       "foo",
			Type:       "STATIC_LIBRARY",
			NameOnDisk: "libfoo.a",
			Install: &fileapi.TargetInstall{
				Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	want := []string{
		"lib/cmake/MyPkg/MyPkgTargets.cmake",
		"lib/libfoo.a",
	}
	if !reflect.DeepEqual(in.InstallFiles, want) {
		t.Errorf("InstallFiles: got %v want %v", in.InstallFiles, want)
	}
	if !reflect.DeepEqual(in.CMakeConfigBundleFiles, []string{"lib/cmake/MyPkg/MyPkgTargets.cmake"}) {
		t.Errorf("CMakeConfigBundleFiles: got %v", in.CMakeConfigBundleFiles)
	}
	if len(in.PublicHeaders) != 0 {
		t.Errorf("PublicHeaders should be empty without FileSets: %v", in.PublicHeaders)
	}
}

func TestBuildInputs_SharedLibraryArtifact(t *testing.T) {
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/Bar",
		ExportName:    "BarTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "bar::@", Name: "bar"}},
	}
	targets := map[string]fileapi.Target{
		"bar::@": {
			Name:       "bar",
			Type:       "SHARED_LIBRARY",
			NameOnDisk: "libbar.so.1",
			Install: &fileapi.TargetInstall{
				Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	// Must contain libbar.so.1 — EmitDeclarative consumes this
	// to populate cc_import.shared_library.
	hasArtifact := false
	for _, p := range in.InstallFiles {
		if p == "lib/libbar.so.1" {
			hasArtifact = true
		}
	}
	if !hasArtifact {
		t.Errorf("InstallFiles missing lib/libbar.so.1: %v", in.InstallFiles)
	}
}

func TestBuildInputs_HeadersFromFileSets(t *testing.T) {
	// Target with a PUBLIC HEADERS FileSet — BuildInputs lifts
	// the headers into PublicHeaders[name] keyed under the
	// install-prefix "include/" path.
	idx := 0
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/Pub",
		ExportName:    "PubTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "pub::@", Name: "pub"}},
	}
	targets := map[string]fileapi.Target{
		"pub::@": {
			Name:       "pub",
			Type:       "STATIC_LIBRARY",
			NameOnDisk: "libpub.a",
			Paths:      fileapi.TargetPaths{Source: "/proj/src"},
			Install: &fileapi.TargetInstall{
				Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
			},
			FileSets: []fileapi.TargetFileSet{{
				Name:            "pub_hdrs",
				Type:            "HEADERS",
				Visibility:      "PUBLIC",
				BaseDirectories: []string{"/proj/src/include"},
			}},
			Sources: []fileapi.TargetSource{
				{Path: "/proj/src/include/pub/api.h", FileSetIndex: &idx},
				{Path: "/proj/src/include/pub/util.h", FileSetIndex: &idx},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	got := in.PublicHeaders["pub"]
	want := []string{"include/pub/api.h", "include/pub/util.h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PublicHeaders[pub]: got %v want %v", got, want)
	}
}

func TestBuildInputs_PrivateHeadersExcluded(t *testing.T) {
	// PRIVATE FILE_SET HEADERS are internal-only; BuildInputs
	// must NOT surface them as public headers (consumers can't
	// see PRIVATE headers — exposing them via cc_import would
	// break the encapsulation cmake's PRIVATE keyword enforces).
	idx := 0
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/X",
		ExportName:    "XTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "x::@", Name: "x"}},
	}
	targets := map[string]fileapi.Target{
		"x::@": {
			Name:       "x",
			Type:       "STATIC_LIBRARY",
			NameOnDisk: "libx.a",
			Install: &fileapi.TargetInstall{
				Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
			},
			FileSets: []fileapi.TargetFileSet{{
				Name:            "priv",
				Type:            "HEADERS",
				Visibility:      "PRIVATE",
				BaseDirectories: []string{"/proj/src/internal"},
			}},
			Sources: []fileapi.TargetSource{
				{Path: "/proj/src/internal/priv.h", FileSetIndex: &idx},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	if hdrs, ok := in.PublicHeaders["x"]; ok && len(hdrs) > 0 {
		t.Errorf("PRIVATE headers leaked: %v", hdrs)
	}
}

func TestBuildInputs_InterfaceLibraryHeadersOnly(t *testing.T) {
	// INTERFACE_LIBRARY has no NameOnDisk (header-only); the
	// installer still records it as an export target and
	// BuildInputs should populate its PublicHeaders without
	// adding an artifact path.
	idx := 0
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/Iface",
		ExportName:    "IfaceTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "iface::@", Name: "iface"}},
	}
	targets := map[string]fileapi.Target{
		"iface::@": {
			Name: "iface",
			Type: "INTERFACE_LIBRARY",
			FileSets: []fileapi.TargetFileSet{{
				Name:            "iface_hdrs",
				Type:            "HEADERS",
				Visibility:      "INTERFACE",
				BaseDirectories: []string{"/proj/src/include"},
			}},
			Sources: []fileapi.TargetSource{
				{Path: "/proj/src/include/iface/h.h", FileSetIndex: &idx},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	if len(in.PublicHeaders["iface"]) != 1 {
		t.Errorf("INTERFACE_LIBRARY headers missing: %v", in.PublicHeaders)
	}
	// The only InstallFile should be the synthesized
	// IfaceTargets.cmake — no artifact.
	for _, p := range in.InstallFiles {
		if p != "lib/cmake/Iface/IfaceTargets.cmake" {
			t.Errorf("INTERFACE_LIBRARY shouldn't add artifact paths; got %q", p)
		}
	}
}

func TestBuildInputs_BundleScriptSynthesized(t *testing.T) {
	// The synthesized "<dest>/<ExportName>.cmake" lands in both
	// InstallFiles (so resolution can see it) and
	// CMakeConfigBundleFiles (so EmitDeclarative emits the
	// bundle filegroup).
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "share/cmake/My",
		ExportName:    "MyTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
	}
	targets := map[string]fileapi.Target{
		"foo::@": {
			Name:       "foo",
			Type:       "STATIC_LIBRARY",
			NameOnDisk: "libfoo.a",
			Install: &fileapi.TargetInstall{
				Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	want := "share/cmake/My/MyTargets.cmake"
	if !contains(in.CMakeConfigBundleFiles, want) {
		t.Errorf("CMakeConfigBundleFiles missing %q: %v", want, in.CMakeConfigBundleFiles)
	}
	if !contains(in.InstallFiles, want) {
		t.Errorf("InstallFiles missing %q: %v", want, in.InstallFiles)
	}
}

func TestBuildInputs_RejectsNonExport(t *testing.T) {
	got := exportshape.BuildInputs(fileapi.DirectoryInstaller{Type: "file"}, nil)
	if !reflect.DeepEqual(got, exportshape.EmitInputs{}) {
		t.Errorf("non-export should yield zero value; got %+v", got)
	}
}

func TestBuildInputs_AbsoluteDestinationSkipsArtifact(t *testing.T) {
	// An absolute install destination escapes the bazel package
	// layout; BuildInputs must NOT emit a cc_import for it. The
	// bundle script still surfaces (downstream consumers might
	// need the cmake-config bundle even when individual targets
	// have weird destinations).
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/Abs",
		ExportName:    "AbsTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "a::@", Name: "a"}},
	}
	targets := map[string]fileapi.Target{
		"a::@": {
			Name:       "a",
			Type:       "STATIC_LIBRARY",
			NameOnDisk: "liba.a",
			Install: &fileapi.TargetInstall{
				Destinations: []fileapi.TargetInstallDest{{Path: "/opt/foo/lib"}},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	for _, p := range in.InstallFiles {
		if p == "/opt/foo/lib/liba.a" {
			t.Errorf("absolute artifact path leaked: %v", in.InstallFiles)
		}
	}
}

func TestBuildInputs_FeedsEmitDeclarative(t *testing.T) {
	// End-to-end shape: BuildInputs's output, passed to
	// EmitDeclarative, produces the expected cc_import + bundle
	// filegroup pair without any pre-materialized install tree.
	idx := 0
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/E2E",
		ExportName:    "E2ETargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "lib::@", Name: "lib"}},
	}
	targets := map[string]fileapi.Target{
		"lib::@": {
			Name:       "lib",
			Type:       "SHARED_LIBRARY",
			NameOnDisk: "liblib.so.1",
			Install: &fileapi.TargetInstall{
				Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
			},
			FileSets: []fileapi.TargetFileSet{{
				Type:            "HEADERS",
				Visibility:      "PUBLIC",
				BaseDirectories: []string{"/proj/include"},
			}},
			Sources: []fileapi.TargetSource{
				{Path: "/proj/include/lib/api.h", FileSetIndex: &idx},
			},
		},
	}
	in := exportshape.BuildInputs(inst, targets)
	in.EmitConfig = true // opt in to the config-mode bundle generation
	out := exportshape.EmitDeclarative(in)
	// Expect: cmake_config_bundle, lib (cc_import), lib_hdrs (filegroup),
	// gen_... (write_file producer for the one bundle file).
	if len(out) != 4 {
		t.Fatalf("want 4 IR targets; got %d: %v", len(out), out)
	}
	var ccImport *ir.Target
	for i := range out {
		if out[i].Kind == ir.KindCCImport {
			ccImport = &out[i]
		}
	}
	if ccImport == nil {
		t.Fatal("no cc_import emitted")
	}
	if ccImport.SharedLibrary != "lib/liblib.so.1" {
		t.Errorf("cc_import.shared_library: got %q want %q", ccImport.SharedLibrary, "lib/liblib.so.1")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
