package cmakerun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadGenexProbe_Empty(t *testing.T) {
	// No cmake-to-bazel.genex/ directory: returns nil, nil.
	got, err := ReadGenexProbe(t.TempDir())
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing probe dir; got %v", got)
	}
}

func TestReadGenexProbe_OneTarget(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "foo")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"type.txt":                          "STATIC_LIBRARY",
		"file.txt":                          "/build/libfoo.a",
		"file_dir.txt":                      "/build",
		"file_name.txt":                     "libfoo.a",
		"interface_INCLUDE_DIRECTORIES.txt": "/src/include;/src/extra",
		"interface_COMPILE_DEFINITIONS.txt": "FOO=1;BAR=2",
		"interface_LINK_LIBRARIES.txt":      "bar;baz",
		"interface_COMPILE_OPTIONS.txt":     "",
		"interface_LINK_OPTIONS.txt":        "",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 probe; got %d (%+v)", len(got), got)
	}
	p := got[0]
	if p.Name != "foo" {
		t.Errorf("Name: %q", p.Name)
	}
	if p.Type != "STATIC_LIBRARY" {
		t.Errorf("Type: %q", p.Type)
	}
	if p.File != "/build/libfoo.a" || p.FileDir != "/build" || p.FileName != "libfoo.a" {
		t.Errorf("File trio: %q / %q / %q", p.File, p.FileDir, p.FileName)
	}
	if p.Interface["INCLUDE_DIRECTORIES"] != "/src/include;/src/extra" {
		t.Errorf("INTERFACE_INCLUDE_DIRECTORIES: %q", p.Interface["INCLUDE_DIRECTORIES"])
	}
	if p.Interface["LINK_LIBRARIES"] != "bar;baz" {
		t.Errorf("INTERFACE_LINK_LIBRARIES: %q", p.Interface["LINK_LIBRARIES"])
	}
	// Empty values are still recorded — distinguishes "probe ran
	// but cmake resolved to empty" from "probe didn't run".
	if _, ok := p.Interface["COMPILE_OPTIONS"]; !ok {
		t.Errorf("INTERFACE_COMPILE_OPTIONS should be present (empty string)")
	}
}

func TestReadGenexProbe_DeterministicOrder(t *testing.T) {
	buildDir := t.TempDir()
	root := filepath.Join(buildDir, "cmake-to-bazel.genex")
	for _, name := range []string{"zeta", "alpha", "middle"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "type.txt"), []byte("STATIC_LIBRARY"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	want := []string{"alpha", "middle", "zeta"}
	for i, p := range got {
		if p.Name != want[i] {
			t.Errorf("probe[%d].Name = %q, want %q", i, p.Name, want[i])
		}
	}
}

func TestReadGenexProbe_InterfaceOnlyTarget(t *testing.T) {
	// INTERFACE_LIBRARY targets get only type.txt + interface_*.txt
	// (no file/file_dir/file_name; no objects).
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "ifacelib")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"type.txt":                          "INTERFACE_LIBRARY",
		"interface_INCLUDE_DIRECTORIES.txt": "/src/include",
		"interface_LINK_LIBRARIES.txt":      "depA",
	} {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 probe; got %d", len(got))
	}
	p := got[0]
	if p.Type != "INTERFACE_LIBRARY" {
		t.Errorf("Type: %q", p.Type)
	}
	if p.File != "" || p.FileDir != "" || p.FileName != "" {
		t.Errorf("INTERFACE_LIBRARY should have no on-disk paths: %+v", p)
	}
	if p.Interface["INCLUDE_DIRECTORIES"] != "/src/include" {
		t.Errorf("INTERFACE_INCLUDE_DIRECTORIES: %q", p.Interface["INCLUDE_DIRECTORIES"])
	}
}

func TestReadGenexProbe_ObjectLibrary(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "objlib")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"type.txt":    "OBJECT_LIBRARY",
		"objects.txt": "/build/CMakeFiles/objlib.dir/a.c.o;/build/CMakeFiles/objlib.dir/b.c.o",
	} {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("want 1 probe")
	}
	if got[0].Objects == "" {
		t.Errorf("Objects empty for OBJECT_LIBRARY: %+v", got[0])
	}
}
