package cmakerun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkInstallPrefix_Empty(t *testing.T) {
	dir := t.TempDir()
	out, err := WalkInstallPrefix(dir)
	if err != nil {
		t.Fatalf("WalkInstallPrefix: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty prefix: got %v want nil", out)
	}
}

func TestWalkInstallPrefix_FilesAndSubdirs(t *testing.T) {
	dir := t.TempDir()
	// Stage a realistic install tree layout.
	files := map[string]string{
		"include/foo.h":                 "// header",
		"include/sub/bar.h":             "// nested",
		"lib/libfoo.a":                  "static archive",
		"lib/cmake/MyPkg/Targets.cmake": "cmake config",
		"share/man/man3/foo.3":          "man page",
	}
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := WalkInstallPrefix(dir)
	if err != nil {
		t.Fatalf("WalkInstallPrefix: %v", err)
	}
	if len(out) != len(files) {
		t.Fatalf("got %d files, want %d (%v)", len(out), len(files), out)
	}
	// Sorted, slash-form, no leading "./".
	wantOrder := []string{
		"include/foo.h",
		"include/sub/bar.h",
		"lib/cmake/MyPkg/Targets.cmake",
		"lib/libfoo.a",
		"share/man/man3/foo.3",
	}
	for i, want := range wantOrder {
		if out[i] != want {
			t.Errorf("out[%d] = %q, want %q (full %v)", i, out[i], want, out)
		}
	}
}

func TestBuildAndInstall_RequiresBuildDir(t *testing.T) {
	// The validation path is reachable without invoking cmake.
	err := BuildAndInstall(nil, InstallOptions{InstallPrefix: "/tmp/p"})
	if err == nil {
		t.Fatal("expected error for missing BuildDir")
	}
}

func TestBuildAndInstall_RequiresInstallPrefix(t *testing.T) {
	err := BuildAndInstall(nil, InstallOptions{BuildDir: "/tmp/b"})
	if err == nil {
		t.Fatal("expected error for missing InstallPrefix")
	}
}
