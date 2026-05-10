package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyDirContents_HappyPath: regular files and nested
// directories round-trip; the destination is reset before copying
// so stale entries don't leak across runs.
func TestCopyDirContents_HappyPath(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// One nested file under a subdir.
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.json"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.json"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pre-existing stale file in dst that the reset must wipe.
	if err := os.WriteFile(filepath.Join(dst, "stale.json"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyDirContents(src, dst); err != nil {
		t.Fatalf("copyDirContents: %v", err)
	}
	for _, want := range []struct {
		path string
		body string
	}{
		{"top.json", "top"},
		{"sub/nested.json", "nested"},
	} {
		got, err := os.ReadFile(filepath.Join(dst, want.path))
		if err != nil {
			t.Errorf("read %s: %v", want.path, err)
			continue
		}
		if string(got) != want.body {
			t.Errorf("%s: got %q, want %q", want.path, got, want.body)
		}
	}
	// Stale entry must be gone (the reset wiped dst before walking src).
	if _, err := os.Stat(filepath.Join(dst, "stale.json")); !os.IsNotExist(err) {
		t.Errorf("stale.json should have been wiped; stat err: %v", err)
	}
}

// TestCopyDirContents_RejectsDangerousDstDir: dstDir comes from
// --out-toolchain-signal-dir, ultimately operator-controlled.
// copyDirContents must not let a misuse like "/" or "." trigger
// a recursive wipe of the operator's filesystem; reject obviously-
// broad paths up front. The guards aren't exhaustive (the operator
// can still pass a too-wide custom path), but they catch the
// foot-guns.
func TestCopyDirContents_RejectsDangerousDstDir(t *testing.T) {
	src := t.TempDir()
	cases := map[string]string{
		"empty":    "",
		"relative": "out",
		"dot":      ".",
		"dot-dot":  "..",
		"root":     "/",
		"home":     "/home",
		"tmp":      "/tmp",
		"etc":      "/etc",
	}
	for label, dst := range cases {
		t.Run(label, func(t *testing.T) {
			err := copyDirContents(src, dst)
			if err == nil {
				t.Errorf("expected error for dst=%q; got nil", dst)
			}
		})
	}
}

// TestCopyDirContents_RejectsSymlinkedSrcDir: filepath.Walk's
// rel == "." early-return masks a symlinked srcDir as "no
// entries to copy" — without an upfront Lstat the function
// would silently produce an empty dstDir. Reject explicitly.
func TestCopyDirContents_RejectsSymlinkedSrcDir(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	dst := filepath.Join(parent, "dst")
	err := copyDirContents(link, dst)
	if err == nil {
		t.Fatal("expected symlinked-srcDir rejection; got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error %q missing 'symlink'", err)
	}
}

// TestCopyDirContents_RejectsNonDirectorySrcDir covers the
// "srcDir is a regular file" misuse (e.g. caller passed --reply
// path instead of the dir). Without the upfront check, walk
// would treat the file as the only entry, hit rel == ".", and
// silently produce an empty dstDir.
func TestCopyDirContents_RejectsNonDirectorySrcDir(t *testing.T) {
	tmp := t.TempDir()
	regular := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(regular, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := copyDirContents(regular, filepath.Join(tmp, "dst"))
	if err == nil {
		t.Fatal("expected non-directory rejection; got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q missing 'not a directory'", err)
	}
}

// TestCopyDirContents_RejectsSymlink: a symlink in the source
// tree (file or directory) is rejected explicitly. Cmake's
// fileapi never writes symlinks, so seeing one means a hostile
// or surprising build dir; refuse rather than silently following
// it (which os.ReadFile / filepath.Walk would otherwise do
// inconsistently).
func TestCopyDirContents_RejectsSymlink(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "outside.json"), []byte("escape"), 0o644); err != nil {
		t.Fatal(err)
	}
	// File symlink pointing outside src.
	if err := os.Symlink(filepath.Join(target, "outside.json"), filepath.Join(src, "link.json")); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}

	err := copyDirContents(src, dst)
	if err == nil {
		t.Fatal("expected symlink rejection; got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error %q missing 'symlink'", err)
	}
}
