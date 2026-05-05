//go:build buildbarn

package cas

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestE2E_SourcePush: buildbarn-gated end-to-end. Pack a tiny
// synthetic tree, upload every blob into the running buildbarn
// CAS via UploadDir (which packs + walks + PutBlob), then read
// one of the file blobs back through GetBlob to prove the wire
// format round-trips through real bb-storage. Invoked by
// `make e2e-source-push`, which stands up + tears down the
// docker-compose stack around it.
//
// CAS endpoint is the buildbarn-up Makefile target's published
// gRPC port. Override with CAS_ADDR if running standalone.
//
// This test replaces internal/casfuse's TestE2E_SourcePush —
// same wire-roundtrip property, expressed against the unified
// internal/cas surface (cas.UploadDir + cas.GRPCStore.GetBlob)
// since the legacy in-process FUSE implementation has been
// retired in favour of bb_clientd.
func TestE2E_SourcePush(t *testing.T) {
	addr := os.Getenv("CAS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8980"
	}

	src := t.TempDir()
	want := []byte("int main(){}\n")
	if err := os.WriteFile(filepath.Join(src, "main.c"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store, err := NewGRPCStore(ctx, GRPCConfig{Endpoint: addr, Insecure: true})
	if err != nil {
		t.Fatalf("dial buildbarn CAS at %s: %v", addr, err)
	}
	defer store.Close()

	if _, err := UploadDir(ctx, store, src); err != nil {
		t.Fatalf("UploadDir against real buildbarn: %v", err)
	}

	// Re-pack to recover the per-file digests (UploadDir only
	// returns the root). The result is byte-stable, so re-packing
	// produces the same digests we just uploaded.
	tree, err := PackDir(src)
	if err != nil {
		t.Fatal(err)
	}
	var fileDigest *Digest
	for _, fn := range tree.Root.Files {
		if fn.Name == "main.c" {
			fileDigest = fn.Digest
			break
		}
	}
	if fileDigest == nil {
		t.Fatal("main.c missing from packed tree")
	}

	got, err := store.GetBlob(ctx, fileDigest)
	if err != nil {
		t.Fatalf("GetBlob(main.c) after push: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("body mismatch after roundtrip: got %q, want %q", got, want)
	}
}
