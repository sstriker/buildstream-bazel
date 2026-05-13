package cas_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/cas"
)

// TestUploadDir asserts cas.UploadDir packs a host directory, uploads
// every blob, and returns a digest that round-trips through
// MaterializeDirectory to byte-identical content.
func TestUploadDir(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.c"), []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "include", "hello.h"), []byte("// hdr\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	digest, err := cas.UploadDir(ctx, store, src)
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if digest == nil || digest.Hash == "" {
		t.Fatalf("UploadDir returned empty digest")
	}

	// Round-trip: materialize back to a fresh dir and confirm contents.
	dst := t.TempDir()
	if err := cas.MaterializeDirectory(ctx, store, digest, dst); err != nil {
		t.Fatalf("MaterializeDirectory: %v", err)
	}
	for rel, want := range map[string][]byte{
		"hello.c":         []byte("int main(){}\n"),
		"include/hello.h": []byte("// hdr\n"),
	} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s body mismatch: got %q want %q", rel, got, want)
		}
	}
}

// TestUploadDir_DeterministicDigest asserts that uploading the same
// content twice yields the same Directory digest. Source identity is
// content-addressed; two hosts that materialize byte-identical trees
// must agree on the digest.
func TestUploadDir_DeterministicDigest(t *testing.T) {
	makeTree := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "a"), []byte("alpha\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "b"), []byte("beta\n"), 0o644)
		return dir
	}

	storeA, _ := cas.NewLocalStore(t.TempDir())
	storeB, _ := cas.NewLocalStore(t.TempDir())

	ctx := context.Background()
	dA, err := cas.UploadDir(ctx, storeA, makeTree(t))
	if err != nil {
		t.Fatalf("UploadDir A: %v", err)
	}
	dB, err := cas.UploadDir(ctx, storeB, makeTree(t))
	if err != nil {
		t.Fatalf("UploadDir B: %v", err)
	}
	if dA.Hash != dB.Hash {
		t.Errorf("digests differ across stores with identical content: %s vs %s", dA.Hash, dB.Hash)
	}
}
