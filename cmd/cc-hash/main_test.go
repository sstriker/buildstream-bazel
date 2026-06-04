package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRun_MatchesCmakeDigest pins the digests against the values cmake's
// file(<ALGO> …) produces for the same bytes (computed independently), and
// asserts the byte-for-byte vtkHashSource header shape.
func TestRun_MatchesCmakeDigest(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte("hello vtk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		algo, digest string
	}{
		{"MD5", "552941bd02d2cc68ef8d409713453711"},
		{"SHA256", "220b59d13876c03195ba4a698664db36eba98c77c8dafbf05b4e58a6bfa9dd85"},
	}
	for _, tc := range cases {
		out := filepath.Join(dir, tc.algo+".h")
		if err := run(in, "MyHash", tc.algo, out); err != nil {
			t.Fatalf("%s: run: %v", tc.algo, err)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		want := "#ifndef MyHash\n #define MyHash \"" + tc.digest + "\"\n#endif\n"
		if string(got) != want {
			t.Errorf("%s header mismatch:\n got: %q\nwant: %q", tc.algo, got, want)
		}
	}
}

func TestRun_AlgorithmCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(in, "H", "sha512", filepath.Join(dir, "o.h")); err != nil {
		t.Errorf("lowercase algorithm should be accepted: %v", err)
	}
}

func TestRun_Rejects(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "o.h")
	if err := run(in, "9bad", "MD5", out); err == nil {
		t.Error("want error for non-identifier --name")
	}
	if err := run(in, "H", "CRC32", out); err == nil {
		t.Error("want error for unsupported --algorithm")
	}
	if err := run("", "H", "MD5", out); err == nil {
		t.Error("want error for missing --input")
	}
}
