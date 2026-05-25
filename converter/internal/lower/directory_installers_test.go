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
	if tgt.Kind != ir.KindFilegroup {
		t.Errorf("Kind: got %v want KindFilegroup", tgt.Kind)
	}
	if tgt.Name != "install_files__include_foo" {
		t.Errorf("Name: %q", tgt.Name)
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

// TestLowerDirectoryInstallers_SkipsNonFileTypes confirms only
// Type=="file" installers participate; Type=="target" is covered by
// per-target Install; Type=="directory" / "export" need their own
// path/classifier handling and are skipped silently.
func TestLowerDirectoryInstallers_SkipsNonFileTypes(t *testing.T) {
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
					{Type: "directory", Destination: "share"},
					{Type: "export", Destination: "lib/cmake/MyPkg", ExportName: "MyPkgTargets"},
				},
			},
		},
	}
	if got := lowerDirectoryInstallers(r); got != nil {
		t.Errorf("expected nil for non-file installer mix; got %v", got)
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
