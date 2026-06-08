package lower

import (
	"encoding/json"
	"strings"
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
	got := lowerDirectoryInstallers(r, false, nil)
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
	got := lowerDirectoryInstallers(r, false, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 filegroup (merged); got %d", len(got))
	}
	// dedup: a.h appears once
	if len(got[0].Srcs) != 2 {
		t.Errorf("Srcs len: %d (want 2; dedup); got %v", len(got[0].Srcs), got[0].Srcs)
	}
}

// TestLowerDirectoryInstallers_SanitizeNameCollisionDisambiguated
// covers the grpc shape: two DISTINCT install destinations that
// sanitize to the same target-name-safe string (include/grpc and
// include/grpc++ both -> install_files__include_grpc, the `++`
// collapses away) must stay distinct targets with disambiguated names,
// not collide (Bazel rejects duplicate names). Each keeps its own
// prefix (the raw destination).
func TestLowerDirectoryInstallers_SanitizeNameCollisionDisambiguated(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "file", Destination: "include/grpc", Paths: rawJSONStrings("/src/grpc/a.h")},
					{Type: "file", Destination: "include/grpc++", Paths: rawJSONStrings("/src/grpcpp/b.h")},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r, false, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 distinct targets; got %d (%v)", len(got), got)
	}
	names := map[string]bool{}
	for _, tg := range got {
		if names[tg.Name] {
			t.Errorf("duplicate target name %q — collision not disambiguated", tg.Name)
		}
		names[tg.Name] = true
	}
	if !names["install_files__include_grpc"] || !names["install_files__include_grpc_2"] {
		t.Errorf("want install_files__include_grpc + _2; got names %v", names)
	}
	// Each target keeps its own (raw) destination as PkgPrefix.
	prefixes := map[string]bool{}
	for _, tg := range got {
		prefixes[tg.PkgPrefix] = true
	}
	if !prefixes["include/grpc"] || !prefixes["include/grpc++"] {
		t.Errorf("each target should keep its raw destination prefix; got %v", prefixes)
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
	if got := lowerDirectoryInstallers(r, false, nil); got != nil {
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
	got := lowerDirectoryInstallers(r, false, nil)
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
// covers install(DIRECTORY) where cmake recorded the path as a plain
// string — the no-trailing-slash "the <dir> itself into DESTINATION"
// shape. Unlike the {"from","to":"."} object (contents-into-dest, which
// strips the whole dir), this strips only the dir's PARENT so the dir
// name survives under the prefix: include/foo/** + strip "include" +
// prefix "include" packages files at include/foo/<rel>.
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
	got := lowerDirectoryInstallers(r, false, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 filegroup; got %d", len(got))
	}
	if got[0].Srcs[0] != "include/foo" {
		t.Errorf("Srcs: %v", got[0].Srcs)
	}
	if !got[0].PkgSrcsGlob {
		t.Errorf("PkgSrcsGlob: got false, want true (directory installer)")
	}
	// No-trailing-slash: strip the PARENT ("include"), not the dir
	// itself, so the "foo" dir name is preserved under the prefix.
	if got[0].PkgStripPrefix != "include" {
		t.Errorf("PkgStripPrefix: got %q want %q (tree-mode strips parent)", got[0].PkgStripPrefix, "include")
	}
}

// TestLowerDirectoryInstallers_DirectoryTree_RootDir covers the
// no-trailing-slash form where the source dir sits at the package root
// (no parent): there's nothing to strip, so the dir name is preserved
// by emitting no strip_prefix at all.
func TestLowerDirectoryInstallers_DirectoryTree_RootDir(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "directory", Destination: "include", Paths: rawJSONStrings("/src/inc")},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r, false, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 target; got %d", len(got))
	}
	if got[0].Srcs[0] != "inc" {
		t.Errorf("Srcs: %v", got[0].Srcs)
	}
	if got[0].PkgStripPrefix != "" {
		t.Errorf("PkgStripPrefix: got %q want empty (root dir, name preserved)", got[0].PkgStripPrefix)
	}
}

// TestLowerDirectoryInstallers_FileRename covers install(FILES ...
// RENAME ...), which the File API records as a {"from","to"} object on
// a Type=="file" installer. Previously the object form was only decoded
// for directory installers, so a renamed file was silently dropped from
// the package. It now lifts to a rules_pkg `renames` entry (dest
// relative to the prefix).
func TestLowerDirectoryInstallers_FileRename(t *testing.T) {
	renameObj := []byte(`{"from":"orig.txt","to":"renamed.txt"}`)
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{Paths: fileapi.CodemodelPaths{Source: "/src"}},
		Directories: map[string]fileapi.Directory{
			"dir.json": {
				Paths: struct {
					Source string `json:"source"`
					Build  string `json:"build"`
				}{Source: "/src"},
				Installers: []fileapi.DirectoryInstaller{
					{Type: "file", Destination: "share/foo", Paths: []json.RawMessage{renameObj}},
				},
			},
		},
	}
	got := lowerDirectoryInstallers(r, false, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 target; got %d", len(got))
	}
	if got[0].Kind != ir.KindPkgFiles {
		t.Errorf("Kind: got %v want KindPkgFiles", got[0].Kind)
	}
	if len(got[0].Srcs) != 1 || got[0].Srcs[0] != "orig.txt" {
		t.Fatalf("Srcs: got %v want [orig.txt]", got[0].Srcs)
	}
	if got[0].PkgPrefix != "share/foo" {
		t.Errorf("PkgPrefix: got %q want share/foo", got[0].PkgPrefix)
	}
	if got[0].PkgSrcsGlob {
		t.Errorf("PkgSrcsGlob: got true, want false (file installer)")
	}
	if got[0].PkgRenames["orig.txt"] != "renamed.txt" {
		t.Errorf("PkgRenames: got %v want {orig.txt: renamed.txt}", got[0].PkgRenames)
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
	if got := lowerDirectoryInstallers(r, false, nil); got != nil {
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
	got := lowerDirectoryInstallers(r, false, nil)
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
	got := lowerDirectoryInstallers(r, false, nil)
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
	got := lowerExportInstallers(r, true)
	// Opt-in (EmitConfig=true): cmake_config_bundle filegroup + foo_import
	// cc_import + the gen_... write_file that generates FooTargets.cmake. Sorted
	// by name: cmake_config_bundle, foo_import, gen_lib_cmake_Foo_FooTargets_cmake.
	if len(got) != 3 {
		t.Fatalf("want 3 IR targets; got %d (%v)", len(got), got)
	}
	if got[0].Name != "cmake_config_bundle" || got[0].Kind != ir.KindFilegroup {
		t.Errorf("first target should be cmake_config_bundle filegroup; got %+v", got[0])
	}
	// The bundle references the write_file producer, not a raw .cmake path.
	if len(got[0].Srcs) != 1 || got[0].Srcs[0] != ":gen_lib_cmake_Foo_FooTargets_cmake" {
		t.Errorf("bundle should reference the write_file producer; got srcs %v", got[0].Srcs)
	}
	if got[1].Name != "foo_import" || got[1].Kind != ir.KindCCImport {
		t.Errorf("second target should be foo_import cc_import; got %+v", got[1])
	}
	if got[1].StaticLibrary != "lib/libfoo.a" {
		t.Errorf("static_library: %q", got[1].StaticLibrary)
	}
	// The generated FooTargets.cmake carries the real imported-target def with
	// IMPORTED_LOCATION under ${_IMPORT_PREFIX} — the form synthprefix parses.
	gen := got[2]
	if gen.Name != "gen_lib_cmake_Foo_FooTargets_cmake" || gen.Kind != ir.KindWriteFile {
		t.Fatalf("third target should be the write_file producer; got %+v", gen)
	}
	body := strings.Join(gen.WriteFileContent, "\n")
	if !strings.Contains(body, "add_library(Foo::foo STATIC IMPORTED)") ||
		!strings.Contains(body, `IMPORTED_LOCATION_NOCONFIG "${_IMPORT_PREFIX}/lib/libfoo.a"`) {
		t.Errorf("generated Targets.cmake missing real imported-target def:\n%s", body)
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
	if got := lowerExportInstallers(r, true); len(got) != 0 {
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
	got := lowerExportInstallers(r, true)
	var bundle *ir.Target
	for i := range got {
		if got[i].Name == "cmake_config_bundle" {
			bundle = &got[i]
		}
	}
	if bundle == nil {
		t.Fatal("cmake_config_bundle filegroup missing")
	}
	// Both bundle scripts present in srcs — as write_file producer refs (the
	// .cmake files are generated, opt-in, by write_file).
	wantSrcs := map[string]bool{
		":gen_lib_cmake_A_ATargets_cmake": true,
		":gen_lib_cmake_B_BTargets_cmake": true,
	}
	for _, s := range bundle.Srcs {
		delete(wantSrcs, s)
	}
	if len(wantSrcs) != 0 {
		t.Errorf("missing srcs from merged bundle: %v (got %v)", wantSrcs, bundle.Srcs)
	}
}

// TestSynthesizeTargetInstallPkgFiles pins install(TARGETS) → pkg_files: a
// built cc_library / cc_binary with an InstallDest gets a pkg_files packaging
// it under that destination; header-only / no-artifact and non-cc kinds are
// skipped; output is sorted + deduped.
func TestSynthesizeTargetInstallPkgFiles(t *testing.T) {
	in := []ir.Target{
		// install(TARGETS) shared lib → pkg_files.
		{Name: "zlib", Kind: ir.KindCCLibrary, InstallDest: "lib", ArtifactName: "libz.so"},
		// install(TARGETS) static lib → pkg_files.
		{Name: "zlibstatic", Kind: ir.KindCCLibrary, InstallDest: "lib", ArtifactName: "libz.a"},
		// install(TARGETS) executable → pkg_files.
		{Name: "minigzip", Kind: ir.KindCCBinary, InstallDest: "bin", ArtifactName: "minigzip"},
		// ABSOLUTE dest (GNUInstallDirs shape) → kept verbatim.
		{Name: "abslib", Kind: ir.KindCCLibrary, InstallDest: "/usr/local/lib", ArtifactName: "libabs.so"},
		// INTERFACE / header-only: has a dest but no artifact → skipped.
		{Name: "zlib_headers", Kind: ir.KindCCLibrary, InstallDest: "lib"},
		// No install dest → skipped.
		{Name: "internal", Kind: ir.KindCCLibrary, ArtifactName: "libinternal.a"},
		// Non-cc kind with a dest (defensive) → skipped.
		{Name: "gen_x", Kind: ir.KindGenrule, InstallDest: "share", ArtifactName: "x"},
		// ".." escapes the install prefix (unsafe / rules_pkg-invalid) → skipped.
		{Name: "escapelib", Kind: ir.KindCCLibrary, InstallDest: "../evil", ArtifactName: "libe.a"},
	}
	got := synthesizeTargetInstallPkgFiles(in)
	want := []struct{ name, src, prefix string }{
		{"install_target__abslib", ":abslib", "/usr/local/lib"},
		{"install_target__minigzip", ":minigzip", "bin"},
		{"install_target__zlib", ":zlib", "lib"},
		{"install_target__zlibstatic", ":zlibstatic", "lib"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d pkg_files, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Name != w.name || g.Kind != ir.KindPkgFiles || g.PkgPrefix != w.prefix ||
			len(g.Srcs) != 1 || g.Srcs[0] != w.src {
			t.Errorf("got[%d] = {name:%q kind:%v srcs:%v prefix:%q}, want {name:%q srcs:[%q] prefix:%q}",
				i, g.Name, g.Kind, g.Srcs, g.PkgPrefix, w.name, w.src, w.prefix)
		}
		if len(g.Visibility) == 0 {
			t.Errorf("got[%d] %q: expected public visibility", i, g.Name)
		}
	}
}

// TestDecodeInstallerPath_GeneratedFileGate pins the produced-output gate on the
// build-dir install(FILES) fallback: a generated FILE under the build dir is
// packaged only when a rule actually produces it (else the pkg_files would
// reference a missing input, e.g. fmt's unlifted fmt-config.cmake); the
// build-dir fallback never applies to install(DIRECTORY).
func TestDecodeInstallerPath_GeneratedFileGate(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"
	raw := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }

	// Produced build-dir file → accepted as its build-relative output name.
	rel, _, ok := decodeInstallerPath(raw("/build/foo.pc"), cmakeSrc, cmakeSrc, cmakeBuild,
		map[string]bool{"foo.pc": true}, "file")
	if !ok || rel != "foo.pc" {
		t.Errorf("produced build-dir file: got (%q, ok=%v), want (\"foo.pc\", true)", rel, ok)
	}

	// Unproduced build-dir file (fmt-config.cmake shape) → dropped.
	if _, _, ok := decodeInstallerPath(raw("/build/foo-config.cmake"), cmakeSrc, cmakeSrc, cmakeBuild,
		map[string]bool{}, "file"); ok {
		t.Errorf("unproduced build-dir file: expected drop, got ok=true")
	}

	// A real source-tree file is unaffected by the gate.
	if rel, _, ok := decodeInstallerPath(raw("include/foo.h"), cmakeSrc, cmakeSrc, cmakeBuild,
		nil, "file"); !ok || rel != "include/foo.h" {
		t.Errorf("source file: got (%q, ok=%v), want (\"include/foo.h\", true)", rel, ok)
	}

	// install(DIRECTORY) build-dir entry → never packaged via the fallback,
	// even when "produced" (a directory has no representable Bazel glob target).
	if _, _, ok := decodeInstallerPath(raw("/build/gen"), cmakeSrc, cmakeSrc, cmakeBuild,
		map[string]bool{"gen": true}, "directory"); ok {
		t.Errorf("build-dir install(DIRECTORY): expected drop, got ok=true")
	}
}
