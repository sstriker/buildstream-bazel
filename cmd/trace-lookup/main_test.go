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

// TestMaterializeConfigBundle_RoundtripThroughLocalStore covers
// the config-bundle leg of the round-2 rendezvous: a bundle
// staged + uploaded under SyntheticConfigDigest is materialized
// back into the caller-supplied path by trace-lookup's
// --out-config-bundle. The bundle's keyspace is distinct from
// the trace's, so this test exercises the second AC key
// independently.
func TestMaterializeConfigBundle_RoundtripThroughLocalStore(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Hand-publish the bundle: stage a flat dir containing
	// cmake-config-bundle.tar, upload, write the AC entry under
	// the config-bundle synthetic digest.
	stage := t.TempDir()
	bundleBody := []byte("synthesized bundle bytes\n")
	if err := os.WriteFile(filepath.Join(stage, "cmake-config-bundle.tar"), bundleBody, 0o644); err != nil {
		t.Fatal(err)
	}
	rootDigest, err := cas.UploadDir(ctx, store, stage)
	if err != nil {
		t.Fatalf("uploaddir: %v", err)
	}
	key, err := tracenorm.SyntheticConfigDigest("srckey-cfg", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateActionResult(ctx, key, &repb.ActionResult{
		OutputDirectories: []*repb.OutputDirectory{
			{Path: "config-bundle", RootDirectoryDigest: rootDigest},
		},
	}); err != nil {
		t.Fatalf("update-ar: %v", err)
	}

	outBundle := filepath.Join(t.TempDir(), "cmake-config-bundle.tar")
	if err := materializeConfigBundle(ctx, store, "srckey-cfg", "", outBundle); err != nil {
		t.Fatalf("materializeConfigBundle: %v", err)
	}
	got, _ := os.ReadFile(outBundle)
	if string(got) != string(bundleBody) {
		t.Errorf("bundle bytes = %q want %q", got, bundleBody)
	}
}

// TestMaterializeConfigBundle_MissProducesEmptyFile asserts the
// miss-side: AC entry absent at the config-bundle key (the trace
// might still hit; they're independent). materializeConfigBundle
// writes a zero-byte file at the destination so Bazel's
// declared-outputs contract holds.
func TestMaterializeConfigBundle_MissProducesEmptyFile(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outBundle := filepath.Join(t.TempDir(), "cmake-config-bundle.tar")
	if err := materializeConfigBundle(ctx, store, "never-published", "", outBundle); err != nil {
		t.Fatalf("materializeConfigBundle: %v", err)
	}
	got, _ := os.ReadFile(outBundle)
	if len(got) != 0 {
		t.Errorf("bundle = %q want empty (miss)", got)
	}
}

// TestMaterializeConfigBundle_BlobEvictedTreatedAsMiss covers
// the AC-survived-but-blob-evicted case for the config bundle:
// the AC entry's RootDirectoryDigest points at a blob that's no
// longer in CAS (Tree-level FindMissing returns it as missing).
// trace-lookup must treat this as a miss (zero-byte output)
// and let the next round's trace_build republish, rather than
// failing the action. Matches the trace-side's
// TestLookup_BlobEvictedTreatedAsMiss contract.
func TestMaterializeConfigBundle_BlobEvictedTreatedAsMiss(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Publish an AR pointing at a digest that's never been
	// uploaded to CAS (synthesize the digest from arbitrary
	// bytes). Mirrors TestLookup_BlobEvictedTreatedAsMiss.
	bogusBody := []byte("not in CAS")
	bogusDigest := cas.DigestOf(bogusBody)
	key, _ := tracenorm.SyntheticConfigDigest("srckey-cfg-evicted", "")
	if err := store.UpdateActionResult(ctx, key, &repb.ActionResult{
		OutputDirectories: []*repb.OutputDirectory{
			{Path: "config-bundle", RootDirectoryDigest: bogusDigest},
		},
	}); err != nil {
		t.Fatal(err)
	}
	outBundle := filepath.Join(t.TempDir(), "cmake-config-bundle.tar")
	if err := materializeConfigBundle(ctx, store, "srckey-cfg-evicted", "", outBundle); err != nil {
		t.Errorf("eviction case should be a clean miss; got %v", err)
	}
	got, _ := os.ReadFile(outBundle)
	if len(got) != 0 {
		t.Errorf("eviction case should produce zero-byte output; got %d bytes", len(got))
	}
}

// TestMaterializeConfigBundle_HitWithoutBundleFileTreatedAsMiss
// covers the "AC hit but the published Directory doesn't carry
// the expected cmake-config-bundle.tar entry" case — defensive
// against a publisher writing a different Directory layout than
// the consumer expects. Falls back to a zero-byte output so
// the action's declared output exists, the consumer treats the
// empty bundle as "no bundle published," and downstream rules
// don't see a half-broken bundle.
func TestMaterializeConfigBundle_HitWithoutBundleFileTreatedAsMiss(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Publish a Directory whose contents are NOT
	// cmake-config-bundle.tar (e.g. it carries something else).
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "something-else.txt"), []byte("not the bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootDigest, err := cas.UploadDir(ctx, store, stage)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := tracenorm.SyntheticConfigDigest("srckey-cfg-mismatched", "")
	if err := store.UpdateActionResult(ctx, key, &repb.ActionResult{
		OutputDirectories: []*repb.OutputDirectory{
			{Path: "config-bundle", RootDirectoryDigest: rootDigest},
		},
	}); err != nil {
		t.Fatal(err)
	}
	outBundle := filepath.Join(t.TempDir(), "cmake-config-bundle.tar")
	if err := materializeConfigBundle(ctx, store, "srckey-cfg-mismatched", "", outBundle); err != nil {
		t.Errorf("mismatched-Directory case should be a clean miss; got %v", err)
	}
	got, _ := os.ReadFile(outBundle)
	if len(got) != 0 {
		t.Errorf("mismatched-Directory case should produce zero-byte output; got %d bytes", len(got))
	}
}

// TestMaterializeHit_CopiesEntriesAndWritesMarker covers the
// action-time materialize mode's happy path: trace.log and
// make-db.txt land at the requested output paths, the marker
// reports "hit", and bytes match what trace-publish wrote.
func TestMaterializeHit_CopiesEntriesAndWritesMarker(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Publish a Directory with both trace.log and make-db.txt
	// — the trace-driven kinds' wire shape.
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "trace.log"), []byte("trace bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "make-db.txt"), []byte("make db bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootDigest, err := cas.UploadDir(ctx, store, stage)
	if err != nil {
		t.Fatalf("uploaddir: %v", err)
	}

	outDir := t.TempDir()
	outTrace := filepath.Join(outDir, "trace.log")
	outMakeDB := filepath.Join(outDir, "make-db.txt")
	outMarker := filepath.Join(outDir, "marker")
	if err := materializeHit(ctx, store, rootDigest, outTrace, outMakeDB, outMarker); err != nil {
		t.Fatalf("materializeHit: %v", err)
	}
	if got, _ := os.ReadFile(outTrace); string(got) != "trace bytes\n" {
		t.Errorf("trace.log = %q want %q", got, "trace bytes\n")
	}
	if got, _ := os.ReadFile(outMakeDB); string(got) != "make db bytes\n" {
		t.Errorf("make-db.txt = %q want %q", got, "make db bytes\n")
	}
	if got, _ := os.ReadFile(outMarker); string(got) != "hit\n" {
		t.Errorf("marker = %q want %q", got, "hit\n")
	}
}

// TestMaterializeHit_AbsentMakeDBProducesEmptyFile covers the
// cmake-round-2-fallback shape (kind:cmake publishes trace.log
// only, no make-db.txt) when the consumer rule still declares
// make-db.txt as an output. The action emits an empty file at
// the destination so Bazel's declared-outputs contract holds;
// consumers treat empty make-db equivalently to no make-db.
func TestMaterializeHit_AbsentMakeDBProducesEmptyFile(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Publish a Directory with only trace.log.
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "trace.log"), []byte("trace only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootDigest, err := cas.UploadDir(ctx, store, stage)
	if err != nil {
		t.Fatalf("uploaddir: %v", err)
	}

	outDir := t.TempDir()
	outTrace := filepath.Join(outDir, "trace.log")
	outMakeDB := filepath.Join(outDir, "make-db.txt")
	outMarker := filepath.Join(outDir, "marker")
	if err := materializeHit(ctx, store, rootDigest, outTrace, outMakeDB, outMarker); err != nil {
		t.Fatalf("materializeHit: %v", err)
	}
	if got, _ := os.ReadFile(outMakeDB); len(got) != 0 {
		t.Errorf("make-db.txt = %q want empty (publisher didn't emit it)", got)
	}
	if got, _ := os.ReadFile(outMarker); string(got) != "hit\n" {
		t.Errorf("marker = %q want %q", got, "hit\n")
	}
}

// TestWriteMissOutputs_ProducesEmptiesAndMarker covers the
// miss-side path (no-CAS-configured / dial-failed / AC-miss
// shapes all route here): every declared output exists post-
// action, and the marker says "miss". The empty trace.log /
// make-db.txt feed the converters' existing "no trace yet" path
// — same shape today's load-time `_trace_repo` empty-fileset
// produces.
func TestWriteMissOutputs_ProducesEmptiesAndMarker(t *testing.T) {
	outDir := t.TempDir()
	outTrace := filepath.Join(outDir, "trace.log")
	outMakeDB := filepath.Join(outDir, "make-db.txt")
	outMarker := filepath.Join(outDir, "marker")
	if err := writeMissOutputs(outTrace, outMakeDB, outMarker); err != nil {
		t.Fatalf("writeMissOutputs: %v", err)
	}
	for _, p := range []string{outTrace, outMakeDB} {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
		}
		if len(body) != 0 {
			t.Errorf("%s = %q want empty", p, body)
		}
	}
	if got, _ := os.ReadFile(outMarker); string(got) != "miss\n" {
		t.Errorf("marker = %q want %q", got, "miss\n")
	}
}

// TestWriteMissOutputs_OmitsMakeDBWhenNotRequested asserts the
// cmake round-2 wire shape (no make-db consumer): when the
// caller doesn't pass --out-make-db, we don't write a phantom
// make-db.txt at the marker's directory. Mirrors how the
// trace_load rule for kind:cmake declares only trace.log +
// marker outputs.
func TestWriteMissOutputs_OmitsMakeDBWhenNotRequested(t *testing.T) {
	outDir := t.TempDir()
	outTrace := filepath.Join(outDir, "trace.log")
	outMarker := filepath.Join(outDir, "marker")
	if err := writeMissOutputs(outTrace, "", outMarker); err != nil {
		t.Fatalf("writeMissOutputs: %v", err)
	}
	// Only trace.log + marker should exist; no spurious make-db.txt.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["trace.log"] || !names["marker"] {
		t.Errorf("missing expected outputs; got %v", names)
	}
	if names["make-db.txt"] {
		t.Errorf("unexpected make-db.txt written when caller didn't request it")
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
