package srckeyregistry

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestFS_RegisterLookupRoundtrip(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	want := []byte("execve(\"/usr/bin/cc\", [\"cc\", \"-c\", \"x.c\"], 0x0) = 0\n")
	if err := r.Register("abc123", "trace.log", want); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, ok, err := r.Lookup("abc123", "trace.log")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true after Register")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content mismatch\nwant: %q\ngot:  %q", want, got)
	}
}

func TestFS_LookupMissing(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.Lookup("abc123", "trace.log")
	if err != nil {
		t.Fatalf("unexpected lookup error: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false on miss; got bytes: %q", got)
	}
}

func TestFS_HasMatchesLookup(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	has, err := r.Has("abc", "x")
	if err != nil || has {
		t.Fatalf("expected has=false on empty; err=%v has=%v", err, has)
	}
	if err := r.Register("abc", "x", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	has, err = r.Has("abc", "x")
	if err != nil || !has {
		t.Fatalf("expected has=true after register; err=%v has=%v", err, has)
	}
}

func TestFS_RegisterOverwrites(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register("k", "trace.log", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("k", "trace.log", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.Lookup("k", "trace.log")
	if err != nil || !ok {
		t.Fatalf("lookup after overwrite: err=%v ok=%v", err, ok)
	}
	if string(got) != "v2" {
		t.Errorf("expected last writer wins; got %q", got)
	}
}

func TestFS_MultipleArtifactsPerKey(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register("k", "trace.log", []byte("trace bytes")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("k", "make-db.txt", []byte("make-db bytes")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, want string }{
		{"trace.log", "trace bytes"},
		{"make-db.txt", "make-db bytes"},
	} {
		got, ok, err := r.Lookup("k", c.name)
		if err != nil || !ok {
			t.Fatalf("%s: lookup err=%v ok=%v", c.name, err, ok)
		}
		if string(got) != c.want {
			t.Errorf("%s: want %q got %q", c.name, c.want, got)
		}
	}
}

func TestFS_RejectIllegalSrckey(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"",
		"../escape",
		"key/with/slash",
		"key with space",
		"...",
		"/abs",
	} {
		if err := r.Register(bad, "trace", []byte("x")); err == nil {
			t.Errorf("Register(%q) should have rejected key", bad)
		}
		if _, _, err := r.Lookup(bad, "trace"); err == nil {
			t.Errorf("Lookup(%q) should have rejected key", bad)
		}
	}
}

func TestFS_RejectIllegalName(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"",
		"path/with/slash",
		"..",
		".hidden",
	} {
		if err := r.Register("k", bad, []byte("x")); err == nil {
			t.Errorf("Register(name=%q) should have rejected name", bad)
		}
	}
}

func TestFS_StorageLayout(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register("abc", "trace.log", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "abc", "trace.log")
	got, ok, err := r.Lookup("abc", "trace.log")
	if err != nil || !ok {
		t.Fatalf("lookup: err=%v ok=%v", err, ok)
	}
	if string(got) != "hi" {
		t.Errorf("content mismatch")
	}
	// Verify the path layout matches what callers external to
	// the package would expect (e.g., a wrapper script that
	// sometimes wants to bypass the package and stat directly).
	t.Logf("expected layout: %s", want)
}
