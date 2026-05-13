package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/tracenorm"
)

// TestLookup_HitReturnsRootDigest verifies the happy path: a
// previously published trace round-trips through synth-key
// derivation and the AC entry's RootDirectoryDigest comes back.
// Mirrors the property trace-publish's roundtrip test asserts
// from the publisher side; here we exercise the consumer side
// in isolation by hand-publishing an AC entry.
func TestLookup_HitReturnsRootDigest(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Hand-publish: stage a flat dir, pack/upload, write AC.
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "trace.log"), []byte("trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootDigest, err := cas.UploadDir(ctx, store, stage)
	if err != nil {
		t.Fatalf("uploaddir: %v", err)
	}
	key, err := tracenorm.SyntheticActionDigest("srckey-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateActionResult(ctx, key, &repb.ActionResult{
		OutputDirectories: []*repb.OutputDirectory{
			{Path: "trace", RootDirectoryDigest: rootDigest},
		},
	}); err != nil {
		t.Fatalf("update-ar: %v", err)
	}

	got, err := lookup(ctx, store, "srckey-1", "")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Fatalf("expected hit; got miss")
	}
	if got.Hash != rootDigest.Hash || got.SizeBytes != rootDigest.SizeBytes {
		t.Errorf("digest mismatch:\n  want %s/%d\n  got  %s/%d",
			rootDigest.Hash, rootDigest.SizeBytes, got.Hash, got.SizeBytes)
	}
}

// TestLookup_MissReturnsNil verifies an unpublished srckey
// resolves to (nil, nil) — the load-bearing "we haven't built
// this srckey yet" signal that triggers the coarse fallback path
// in the converter.
func TestLookup_MissReturnsNil(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := lookup(ctx, store, "never-published", "")
	if err != nil {
		t.Errorf("miss should not be an error; got %v", err)
	}
	if got != nil {
		t.Errorf("miss should produce nil digest; got %s/%d", got.Hash, got.SizeBytes)
	}
}

// TestLookup_BlobEvictedTreatedAsMiss covers the AC-eviction
// resilience case: AC entry survived but the trace blob got
// evicted from CAS. Lookup must report this as a clean miss
// (not an error) so round-1 republishes rather than failing
// the build.
func TestLookup_BlobEvictedTreatedAsMiss(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Publish a normal AR pointing at a digest that's never
	// landed in CAS (synthesize a digest of arbitrary bytes).
	bogusBody := []byte("not actually in cas")
	bogusDigest := cas.DigestOf(bogusBody)
	key, _ := tracenorm.SyntheticActionDigest("srckey-evicted", "")
	if err := store.UpdateActionResult(ctx, key, &repb.ActionResult{
		OutputDirectories: []*repb.OutputDirectory{
			{Path: "trace", RootDirectoryDigest: bogusDigest},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := lookup(ctx, store, "srckey-evicted", "")
	if err != nil {
		t.Errorf("eviction case should be a clean miss; got %v", err)
	}
	if got != nil {
		t.Errorf("eviction case should report miss; got digest %s/%d", got.Hash, got.SizeBytes)
	}
}
