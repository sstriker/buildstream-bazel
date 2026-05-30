package lower

import (
	"encoding/json"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func rawJSONStrings(values ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(values))
	for i, v := range values {
		b, _ := json.Marshal(v)
		out[i] = b
	}
	return out
}

// TestLowerDirectoryInstallers_FileInstaller covers the canonical
// shape: one install(FILES include/foo.h include/bar.h DESTINATION
// include/foo) call surfaces as a single filegroup.
func TestLowerDirectoryInstallers_FileInstaller(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"directory.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{{
					Type:        "file",
					Destination: "include/foo",
					Paths:       rawJSONStrings("/src/include/foo.h", "/src/include/bar.h"),
				}},
			},
		},
	}
	got := lowerDirectoryInstallers(r)
	if len(got) != 1 {
		t.Fatalf("want 1 filegroup; got %d (%+v)", len(got), got)
	}
	tgt := got[0]
	if tgt.Kind != ir.KindPkgFiles {
		t.Errorf("Kind: got %v want KindPkgFiles", tgt.Kind)
	}
	if tgt.Name != "install_files__include_foo" {
		t.Errorf("Name: %q", tgt.Name)
	}
	if tgt.PkgPrefix != "include/foo" {
		t.Errorf("PkgPrefix: got %q want %q", tgt.PkgPrefix, "include/foo")
	}
	wantSrcs := []string{"include/bar.h", "include/foo.h"}
	if len(tgt.Srcs) != len(wantSrcs) {
		t.Fatalf("Srcs: %v", tgt.Srcs)
	}
	for i, s := range wantSrcs {
		if tgt.Srcs[i] != s {
			t.Errorf("Srcs[%d]: got %q want %q", i, tgt.Srcs[i], s)
		}
	}
	// install(FILES) srcs are individual files — they stay a literal
	// list (no glob, no strip_prefix). Only install(DIRECTORY) globs.
	if tgt.PkgSrcsGlob {
		t.Errorf("PkgSrcsGlob: got true, want false (file installer keeps literal srcs)")
	}
	if tgt.PkgStripPrefix != "" {
		t.Errorf("PkgStripPrefix: got %q, want empty (file installer)", tgt.PkgStripPrefix)
	}
}

// TestLowerDirectoryInstallers_GroupsByDestination covers multiple
// install(FILES ... DESTINATION X) calls to the same destination —
// they merge into one filegroup with deduped srcs.
func TestLowerDirectoryInstallers_GroupsByDestination(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "file", Destination: "include", Paths: rawJSONStrings("/src/a.h")},
					{Type: "file", Destination: "include", Paths: rawJSONStrings("/src/b.h", "/src/a.h")},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r)
	if len(got) != 1 {
		t.Fatalf("want 1 filegroup (merged); got %d", len(got))
	}
	// dedup: a.h appears once
	if len(got[0].Srcs) != 2 {
		t.Errorf("Srcs len: %d (want 2; dedup); got %v", len(got[0].Srcs), got[0].Srcs)
	}
}

// TestLowerDirectoryInstallers_SkipsNonFileNonDirectoryTypes
// confirms install(TARGETS) and install(EXPORT) are skipped (covered
// by per-target Install + Phase 6's classifier respectively).
func TestLowerDirectoryInstallers_SkipsNonFileNonDirectoryTypes(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "target", Destination: "lib", Paths: rawJSONStrings("libfoo.a")},
					{Type: "export", Destination: "lib/cmake/MyPkg", ExportName: "MyPkgTargets"},
				},
			},
		},
	}
	if got := lowerDirectoryInstallers(r); got != nil {
		t.Errorf("expected nil for target/export only; got %v", got)
	}
}

// TestLowerDirectoryInstallers_DirectoryInstaller_ObjectPath covers
// install(DIRECTORY) with the {"from": ..., "to": ...} object path
// schema. The "from" path projects into the filegroup src; the
// emitted target is named install_directory__<dest>.
func TestLowerDirectoryInstallers_DirectoryInstaller_ObjectPath(t *testing.T) {
	pathObj := []byte(`{"from":"/src/share/data","to":""}`)
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "directory", Destination: "share/myapp", Paths: []json.RawMessage{pathObj}},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r)
	if len(got) != 1 {
		t.Fatalf("want 1 filegroup; got %d", len(got))
	}
	if got[0].Kind != ir.KindPkgFiles {
		t.Errorf("Kind: got %v want KindPkgFiles", got[0].Kind)
	}
	if got[0].Name != "install_directory__share_myapp" {
		t.Errorf("Name: %q", got[0].Name)
	}
	if got[0].PkgPrefix != "share/myapp" {
		t.Errorf("PkgPrefix: got %q want %q", got[0].PkgPrefix, "share/myapp")
	}
	if len(got[0].Srcs) != 1 || got[0].Srcs[0] != "share/data" {
		t.Errorf("Srcs: %v", got[0].Srcs)
	}
	// install(DIRECTORY) must glob the source dir's contents (a bare
	// directory in pkg_files srcs doesn't package its files) and strip
	// the source dir so files land at "<dest>/<rel>".
	if !got[0].PkgSrcsGlob {
		t.Errorf("PkgSrcsGlob: got false, want true (directory installer)")
	}
	if got[0].PkgStripPrefix != "share/data" {
		t.Errorf("PkgStripPrefix: got %q want %q", got[0].PkgStripPrefix, "share/data")
	}
}

// TestLowerDirectoryInstallers_DirectoryInstaller_StringShortForm
// covers install(DIRECTORY) where cmake recorded the path as a
// plain string (DESTINATION-implicit "to"). Same fallback path
// the file installer uses.
func TestLowerDirectoryInstallers_DirectoryInstaller_StringShortForm(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "directory", Destination: "include", Paths: rawJSONStrings("/src/include/foo")},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r)
	if len(got) != 1 {
		t.Fatalf("want 1 filegroup; got %d", len(got))
	}
	if got[0].Srcs[0] != "include/foo" {
		t.Errorf("Srcs: %v", got[0].Srcs)
	}
}

// TestLowerDirectoryInstallers_SkipsExcludeAndOptional confirms
// EXCLUDE_FROM_ALL and OPTIONAL installers don't emit — they're
// part of the conditional install set.
func TestLowerDirectoryInstallers_SkipsExcludeAndOptional(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "file", Destination: "include", Paths: rawJSONStrings("/src/foo.h"), IsExcludeFromAll: true},
					{Type: "file", Destination: "share", Paths: rawJSONStrings("/src/bar.txt"), IsOptional: true},
				},
			},
		},
	}
	if got := lowerDirectoryInstallers(r); got != nil {
		t.Errorf("expected nil for excluded/optional installers; got %v", got)
	}
}

// TestLowerDirectoryInstallers_SkipsOutOfTreePaths drops absolute
// paths outside the source root — they'd produce unresolvable
// Bazel labels.
func TestLowerDirectoryInstallers_SkipsOutOfTreePaths(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "file", Destination: "include", Paths: rawJSONStrings("/usr/share/elsewhere/foo.h", "/src/local.h")},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r)
	if len(got) != 1 {
		t.Fatalf("want 1 filegroup; got %d", len(got))
	}
	if len(got[0].Srcs) != 1 || got[0].Srcs[0] != "local.h" {
		t.Errorf("out-of-tree path should be dropped; got %v", got[0].Srcs)
	}
}

// TestLowerDirectoryInstallers_DeterministicOrder confirms the
// returned slice is sorted by destination name so emit produces
// byte-stable BUILD output.
func TestLowerDirectoryInstallers_DeterministicOrder(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "file", Destination: "zeta", Paths: rawJSONStrings("/src/z.h")},
					{Type: "file", Destination: "alpha", Paths: rawJSONStrings("/src/a.h")},
					{Type: "file", Destination: "middle", Paths: rawJSONStrings("/src/m.h")},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r)
	wantOrder := []string{"install_files__alpha", "install_files__middle", "install_files__zeta"}
	if len(got) != len(wantOrder) {
		t.Fatalf("len: %d", len(got))
	}
	for i, want := range wantOrder {
		if got[i].Name != want {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, want)
		}
	}
}

// TestSanitizeDestination covers the destination → target-name-safe
// translation: alphanumerics survive; everything else collapses to
// a single underscore; leading/trailing underscores are trimmed.
func TestSanitizeDestination(t *testing.T) {
	cases := []struct{ in, want string }{
		{"include", "include"},
		{"include/foo", "include_foo"},
		{"lib/cmake/MyPkg", "lib_cmake_MyPkg"},
		{"lib//cmake//MyPkg", "lib_cmake_MyPkg"}, // collapsed slashes
		{"share/man/man3", "share_man_man3"},
		{"docs/cmake-2.0", "docs_cmake_2_0"},
		{"./relative", "relative"}, // path.Clean strips
		{"/abs/path/", "abs_path"}, // leading + trailing underscore trimmed
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := sanitizeDestination(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeDestination(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLowerExportInstallers_DeclarativeWiresThroughEmitDeclarative
// pins the Phase 6 wire-up: a declarative install(EXPORT) shape
// produces the suffixed cc_import + cmake_config_bundle filegroup
// + Phase 6 tag — all without running cmake --install.
func TestLowerExportInstallers_DeclarativeWiresThroughEmitDeclarative(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
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
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{{
					Type:          "export",
					Destination:   "lib/cmake/Foo",
					ExportName:    "FooTargets",
					ExportTargets: []fileapi.ExportTarget{{Id: "foo::@"}},
				}},
			},
		},
	}
	got := lowerExportInstallers(r)
	// Two targets: cmake_config_bundle filegroup + foo_import
	// cc_import. Sorted by name: cmake_config_bundle, foo_import.
	if len(got) != 2 {
		t.Fatalf("want 2 IR targets; got %d (%v)", len(got), got)
	}
	if got[0].Name != "cmake_config_bundle" || got[0].Kind != ir.KindFilegroup {
		t.Errorf("first target should be cmake_config_bundle filegroup; got %+v", got[0])
	}
	if got[1].Name != "foo_import" || got[1].Kind != ir.KindCCImport {
		t.Errorf("second target should be foo_import cc_import; got %+v", got[1])
	}
	if got[1].StaticLibrary != "lib/libfoo.a" {
		t.Errorf("static_library: %q", got[1].StaticLibrary)
	}
	// Phase 6 tag must be present so cmakecfg's bundle synthesizer
	// can de-duplicate the IMPORTED entry.
	want := "cmake-codegen-install-export-import"
	found := false
	for _, tag := range got[1].Tags {
		if tag == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("foo_import missing %q tag; got tags = %v", want, got[1].Tags)
	}
}

// TestLowerExportInstallers_ImperativeStaysOnFallback verifies the
// non-declarative residue: an installer that doesn't match the
// classifier's preconditions (here: EXCLUDE_FROM_ALL) yields no IR.
// The lowering returns the bundle to the round-2 _install_tree_extract
// fallback by not emitting anything here.
func TestLowerExportInstallers_ImperativeStaysOnFallback(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
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
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{{
					Type:             "export",
					Destination:      "lib/cmake/Foo",
					ExportName:       "FooTargets",
					ExportTargets:    []fileapi.ExportTarget{{Id: "foo::@"}},
					IsExcludeFromAll: true,
				}},
			},
		},
	}
	if got := lowerExportInstallers(r); len(got) != 0 {
		t.Errorf("imperative bundle should emit nothing; got %+v", got)
	}
}

// TestLowerExportInstallers_BundleFilegroupsMerge covers the dedup:
// two declarative install(EXPORT) calls in the same package both
// produce a "cmake_config_bundle" filegroup; the wire merges their
// srcs into one to avoid a "target already declared" Bazel error.
func TestLowerExportInstallers_BundleFilegroupsMerge(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Targets: map[string]fileapi.Target{
			"a::@": {Name: "a", Type: "STATIC_LIBRARY", NameOnDisk: "liba.a", Install: &fileapi.TargetInstall{Destinations: []fileapi.TargetInstallDest{{Path: "lib"}}}},
			"b::@": {Name: "b", Type: "STATIC_LIBRARY", NameOnDisk: "libb.a", Install: &fileapi.TargetInstall{Destinations: []fileapi.TargetInstallDest{{Path: "lib"}}}},
		},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "export", Destination: "lib/cmake/A", ExportName: "ATargets", ExportTargets: []fileapi.ExportTarget{{Id: "a::@"}}},
					{Type: "export", Destination: "lib/cmake/B", ExportName: "BTargets", ExportTargets: []fileapi.ExportTarget{{Id: "b::@"}}},
				},
			},
		},
	}
	got := lowerExportInstallers(r)
	var bundle *ir.Target
	for i := range got {
		if got[i].Name == "cmake_config_bundle" {
			bundle = &got[i]
		}
	}
	if bundle == nil {
		t.Fatal("cmake_config_bundle filegroup missing")
	}
	// Both bundle scripts present in srcs.
	wantSrcs := map[string]bool{
		"lib/cmake/A/ATargets.cmake": true,
		"lib/cmake/B/BTargets.cmake": true,
	}
	for _, s := range bundle.Srcs {
		delete(wantSrcs, s)
	}
	if len(wantSrcs) != 0 {
		t.Errorf("missing srcs from merged bundle: %v (got %v)", wantSrcs, bundle.Srcs)
	}
}
